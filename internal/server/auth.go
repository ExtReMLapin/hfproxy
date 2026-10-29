package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-ldap/ldap/v3"
	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/keyring"
	"golang.org/x/crypto/argon2"
	"golang.org/x/oauth2"
)

const loginPage = `<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connexion · hfproxy</title><link rel="stylesheet" href="/assets/app.css"></head><body><main class="login"><div class="brand"><img class="mark" src="/hfproxy-mark.svg" alt="">hfproxy</div><p class="muted">Connectez-vous pour accéder au proxy et à son cache de modèles.</p>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{if .Local}}<form method="post" action="/login"><input type="hidden" name="csrf_token" value="{{.CSRF}}"><label for="username">Identifiant</label><input id="username" name="username" autocomplete="username" required><label for="password">Mot de passe</label><input id="password" type="password" name="password" autocomplete="current-password" required><button type="submit">Se connecter</button></form>{{end}}{{if .OIDC}}{{if .Local}}<div class="divider"></div>{{end}}<a class="button" href="/auth/login">Continuer avec SSO</a>{{end}}{{if not .Local}}{{if not .OIDC}}<p class="muted">Aucun fournisseur d’authentification n’est configuré.</p>{{end}}{{end}}</main></body></html>`

type loginData struct {
	Error string
	CSRF  string
	Local bool
	OIDC  bool
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if p, err := s.authenticate(r); err == nil && p != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data := loginData{Local: s.currentConfig().AuthMode == "local" || s.currentConfig().LDAPURL != "", OIDC: s.currentOIDCConfig() != nil}
	if r.Method == http.MethodGet {
		cookie, err := r.Cookie("hfproxy_login_csrf")
		if err != nil || cookie.Value == "" {
			cookie = &http.Cookie{Name: "hfproxy_login_csrf", Value: randomSecret(24), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(s.currentConfig().PublicURL, "https://"), MaxAge: 600}
			http.SetCookie(w, cookie)
		}
		data.CSRF = cookie.Value
		s.renderLogin(w, data, http.StatusOK)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, data, http.StatusBadRequest)
		return
	}
	csrfCookie, err := r.Cookie("hfproxy_login_csrf")
	if err != nil || !constantEqual(csrfCookie.Value, r.FormValue("csrf_token")) {
		s.logger.Warn("sign-in refused: the form token is missing or does not match", "request_id", requestID(r.Context()), "client_ip", s.loginClientIP(r))
		data.Error = "La requête a expiré. Rechargez la page puis réessayez."
		s.renderLogin(w, data, http.StatusForbidden)
		return
	}
	if retryAfter, ok := s.allowLoginAttempt(r, r.FormValue("username")); !ok {
		s.logger.Warn("sign-in refused: too many attempts", "request_id", requestID(r.Context()), "client_ip", s.loginClientIP(r), "retry_after_seconds", retryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeProblem(w, r, http.StatusTooManyRequests, "Too many login attempts", "Retry later before attempting to sign in again.")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	var userID string
	var authErr error
	provider := "local"
	if s.currentConfig().AuthMode == "local" {
		s.loginMu.Lock()
		if s.loginHashSlots == nil {
			s.loginHashSlots = make(chan struct{}, 4)
		}
		slots := s.loginHashSlots
		s.loginMu.Unlock()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-r.Context().Done():
			return
		}
		var storedHash, status string
		userID, storedHash, status, authErr = s.db.PasswordLogin(r.Context(), username)
		if !verifyPassword(localLoginHash(storedHash, status, authErr), password) {
			authErr = sql.ErrNoRows
		}
	} else {
		authErr = sql.ErrNoRows
	}
	if authErr != nil && s.currentConfig().LDAPURL != "" {
		userID, err = s.authenticateLDAP(r.Context(), username, password)
		provider = "ldap"
	} else {
		err = authErr
	}
	if err != nil || userID == "" {
		// The name typed is not logged: people sometimes type their password in
		// that field. The audit table keeps a capped copy for investigations.
		s.logger.Warn("sign-in failed", "request_id", requestID(r.Context()), "client_ip", s.loginClientIP(r), "provider", provider)
		s.recordLoginFailure(r.Context(), username)
		s.audit(r.Context(), "", "auth.login_failed", "user", auditText(username), map[string]any{"provider": provider})
		data.Error = "Identifiant ou mot de passe incorrect."
		s.renderLogin(w, data, http.StatusUnauthorized)
		return
	}
	if _, err := s.createSession(r.Context(), userID, w, r); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Session error", "Unable to create a session.")
		return
	}
	s.audit(r.Context(), userID, "auth.login", "user", userID, map[string]any{"provider": provider})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func localLoginHash(storedHash, status string, lookupErr error) string {
	if lookupErr != nil || status != "active" {
		return dummyPasswordHash
	}
	return storedHash
}

func (s *Server) renderLogin(w http.ResponseWriter, data loginData, status int) {
	t, err := template.New("login").Parse(loginPage)
	if err != nil {
		http.Error(w, "login page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = t.Execute(w, data)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.checkCSRF(r) {
		writeProblem(w, r, http.StatusForbidden, "CSRF validation failed", "Reload the page and retry the action.")
		return
	}
	cookie, err := r.Cookie("hfproxy_session")
	var actorID string
	if err == nil && cookie.Value != "" {
		candidates := s.keyRing().HashCandidates(cookie.Value)
		actorID = s.db.SessionOwner(r.Context(), candidates)
		if err := s.db.DeleteSessions(r.Context(), candidates); err != nil {
			s.logger.Error("session could not be deleted at logout; it stays valid until it expires", "request_id", requestID(r.Context()))
		}
	}
	s.forgetCredentials(r.Context())
	s.audit(r.Context(), actorID, "auth.logout", "user", actorID, nil)
	clearCookie(w, "hfproxy_session")
	clearCookie(w, "hfproxy_csrf")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type oidcState struct {
	State    string    `json:"state"`
	Nonce    string    `json:"nonce"`
	Verifier string    `json:"verifier"`
	Expires  time.Time `json:"expires"`
}

func (s *Server) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.currentOIDCConfig() == nil {
		http.NotFound(w, r)
		return
	}
	state := oidcState{State: randomSecret(24), Nonce: randomSecret(24), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(10 * time.Minute)}
	encoded, _ := json.Marshal(state)
	http.SetCookie(w, &http.Cookie{Name: "hfproxy_oidc", Value: base64.RawURLEncoding.EncodeToString(encoded), Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.currentConfig().OIDCRedirectURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	url := s.currentOIDCConfig().AuthCodeURL(state.State, oauth2.S256ChallengeOption(state.Verifier), oauth2.SetAuthURLParam("nonce", state.Nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.currentOIDCConfig() == nil || s.currentOIDCVerifier() == nil {
		http.NotFound(w, r)
		return
	}
	// Clear the state cookie before any redirect: headers set after
	// http.Redirect has written the response are silently dropped.
	http.SetCookie(w, &http.Cookie{Name: "hfproxy_oidc", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.currentConfig().OIDCRedirectURL, "https://"), SameSite: http.SameSiteLaxMode})
	cookie, err := r.Cookie("hfproxy_oidc")
	if err != nil {
		http.Redirect(w, r, "/login?error=invalid_state", http.StatusSeeOther)
		return
	}
	data, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	var state oidcState
	if err != nil || json.Unmarshal(data, &state) != nil || time.Now().After(state.Expires) || state.State == "" || state.State != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/login?error=invalid_state", http.StatusSeeOther)
		return
	}
	token, err := s.currentOIDCConfig().Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil {
		s.logger.Warn("OIDC code exchange failed", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 200))
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	idToken, err := s.currentOIDCVerifier().Verify(r.Context(), rawIDToken)
	if err != nil {
		s.logger.Warn("OIDC ID token validation failed", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 200))
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	var claims struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Nonce         string `json:"nonce"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" || !constantEqual(claims.Nonce, state.Nonce) {
		http.Redirect(w, r, "/login?error=login_failed", http.StatusSeeOther)
		return
	}
	userID, err := s.oidcUser(r.Context(), idToken.Issuer, claims)
	if err != nil {
		s.audit(r.Context(), "", "auth.login_failed", "identity", idToken.Subject, map[string]any{"provider": "oidc"})
		http.Redirect(w, r, "/login?error=provisioning_disabled", http.StatusSeeOther)
		return
	}
	if _, err := s.createSession(r.Context(), userID, w, r); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Session error", "Unable to create a session.")
		return
	}
	s.audit(r.Context(), userID, "auth.login", "user", userID, map[string]any{"provider": "oidc"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) oidcUser(ctx context.Context, issuer string, claims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Nonce         string `json:"nonce"`
}) (string, error) {
	account, err := s.db.FindExternalIdentity(ctx, issuer, claims.Subject)
	if err == nil {
		if account.Status != "active" {
			return "", fmt.Errorf("account disabled")
		}
		return account.UserID, nil
	}
	if !errors.Is(err, db.ErrNotFound) || !s.currentConfig().OIDCAutoProvision || !claims.EmailVerified {
		return "", fmt.Errorf("identity is not provisioned")
	}
	email, err := mail.ParseAddress(claims.Email)
	if err != nil || !claims.EmailVerified {
		return "", fmt.Errorf("verified email is required for provisioning")
	}
	userID := db.NewID()
	if err := s.db.ProvisionExternalUser(ctx, userID, usernameFromEmail(email.Address), email.Address, claims.Name, "oidc", issuer, claims.Subject); err != nil {
		return "", err
	}
	s.audit(ctx, userID, "user.provision", "user", userID, map[string]any{"provider": "oidc"})
	return userID, nil
}

func (s *Server) authenticateLDAP(ctx context.Context, username, password string) (string, error) {
	if s.currentConfig().LDAPURL == "" || username == "" || password == "" {
		return "", fmt.Errorf("LDAP authentication is not available")
	}
	conn, err := ldap.DialURL(s.currentConfig().LDAPURL,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
		ldap.DialWithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}))
	if err != nil {
		return "", fmt.Errorf("LDAP connection failed")
	}
	defer conn.Close()
	conn.SetTimeout(5 * time.Second)
	parsedURL, err := url.Parse(s.currentConfig().LDAPURL)
	if err != nil {
		return "", fmt.Errorf("LDAP configuration is invalid")
	}
	if parsedURL.Scheme == "ldap" && s.currentConfig().LDAPStartTLS {
		if err := conn.StartTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsedURL.Hostname()}); err != nil {
			return "", fmt.Errorf("LDAP StartTLS failed")
		}
	}
	if s.currentConfig().LDAPBindDN != "" {
		if err := conn.Bind(s.currentConfig().LDAPBindDN, s.currentConfig().LDAPBindPassword); err != nil {
			return "", fmt.Errorf("LDAP service bind failed")
		}
	}
	filter := strings.ReplaceAll(s.currentConfig().LDAPUserFilter, "{username}", ldap.EscapeFilter(username))
	request := ldap.NewSearchRequest(s.currentConfig().LDAPBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 2, 5, false, filter, ldapUserAttributes, nil)
	result, err := conn.Search(request)
	if err != nil || len(result.Entries) != 1 {
		return "", fmt.Errorf("LDAP user lookup failed")
	}
	entry := result.Entries[0]
	if err := conn.Bind(entry.DN, password); err != nil {
		return "", fmt.Errorf("LDAP credentials rejected")
	}
	if ldapAccountDisabled(entry) {
		return "", errLDAPAccountDisabled
	}
	issuer := s.currentConfig().LDAPURL
	stable := ldapStableSubject(entry)
	subject := stable
	if subject == "" {
		subject = entry.DN
	}
	account, err := s.findLDAPIdentity(ctx, issuer, stable, entry.DN)
	if err == nil {
		if account.Status != "active" {
			return "", fmt.Errorf("account disabled")
		}
		s.refreshLDAPGroups(ctx, conn, account.UserID, entry.DN)
		return account.UserID, nil
	}
	if !errors.Is(err, db.ErrNotFound) || !s.currentConfig().LDAPAutoProvision {
		return "", fmt.Errorf("LDAP identity is not provisioned")
	}
	localUsername := usernameFromLogin(username)
	email := entry.GetAttributeValue("mail")
	displayName := entry.GetAttributeValue("displayName")
	if displayName == "" {
		displayName = entry.GetAttributeValue("cn")
	}
	if len(email) > 254 {
		email = ""
	}
	if len(displayName) > 200 {
		displayName = username
	}
	userID := db.NewID()
	if err := s.db.ProvisionExternalUser(ctx, userID, localUsername, email, displayName, "ldap", issuer, subject); err != nil {
		var stage *db.StageError
		if errors.As(err, &stage) && stage.Stage == "identity" {
			return "", fmt.Errorf("record LDAP identity failed")
		}
		if errors.As(err, &stage) {
			return "", fmt.Errorf("provision LDAP user failed")
		}
		return "", err
	}
	s.audit(ctx, userID, "user.provision", "user", userID, map[string]any{"provider": "ldap"})
	s.refreshLDAPGroups(ctx, conn, userID, entry.DN)
	return userID, nil
}

// refreshLDAPGroups mirrors the directory groups when group sync is on. A
// failure is logged and never blocks the sign-in; memberships stay as they were.
func (s *Server) refreshLDAPGroups(ctx context.Context, conn *ldap.Conn, userID, dn string) {
	if !s.currentConfig().LDAPGroupSync {
		return
	}
	// The connection is bound as the user; search as the service account, which
	// is the one allowed to read groups.
	if cfg := s.currentConfig(); cfg.LDAPBindDN != "" {
		if err := conn.Bind(cfg.LDAPBindDN, cfg.LDAPBindPassword); err != nil {
			s.logger.Warn("LDAP service bind failed before reading groups; memberships unchanged", "request_id", requestID(ctx))
			return
		}
	}
	names, err := s.ldapGroupNames(conn, dn)
	if err != nil {
		s.logger.Warn("LDAP groups could not be read; memberships unchanged", "request_id", requestID(ctx))
		return
	}
	if err := s.syncLDAPGroups(ctx, userID, names); err != nil {
		s.logger.Warn("LDAP groups could not be synchronised", "request_id", requestID(ctx))
	}
}

func usernameFromLogin(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		}
	}
	base := strings.Trim(b.String(), "._-")
	if len(base) > 40 {
		base = base[:40]
	}
	if base == "" {
		base = "user"
	}
	return base + "-" + randomSecret(4)
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// HashToken is the original token hash, HMAC-SHA256 keyed with the raw session
// secret. New tokens use keys derived from the secret (see internal/keyring);
// this form is still accepted so existing tokens keep working.
func HashToken(token, pepper string) []byte { return keyring.LegacyHash(token, pepper) }

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", fmt.Errorf("password must contain 12 to 1024 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	const memory = 64 * 1024
	const iterations = 3
	const parallelism = 1
	hash := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, 32)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

var dummyPasswordHash = func() string {
	hash, err := HashPassword("hfproxy-dummy-password")
	if err != nil {
		panic(err)
	}
	return hash
}()

// allowLoginAttempt applies the shared (PostgreSQL) login limits. Without a
// database, or if the shared counters cannot be read, it falls back to
// per-process counters rather than leaving the endpoint unprotected.
func (s *Server) allowLoginAttempt(r *http.Request, username string) (int, bool) {
	if s.db != nil && s.db.SQL != nil {
		retryAfter, allowed, err := s.allowLoginAttemptShared(r.Context(), s.loginClientIP(r), username)
		if err == nil {
			return retryAfter, allowed
		}
		if s.logger != nil {
			s.logger.Warn("shared login limits unavailable; using local counters", "class", fmt.Sprintf("%T", err))
		}
	}
	return s.allowLoginAttemptLocal(r, username)
}

func (s *Server) allowLoginAttemptLocal(r *http.Request, username string) (int, bool) {
	host := s.loginClientIP(r)
	keys := []string{"ip:" + host, "user:" + strings.ToLower(strings.TrimSpace(username))}
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginBuckets == nil {
		s.loginBuckets = make(map[string]loginBucket)
	}
	for key, bucket := range s.loginBuckets {
		if now.Sub(bucket.window) >= time.Minute {
			delete(s.loginBuckets, key)
		}
	}
	missing := 0
	for _, key := range keys {
		if _, exists := s.loginBuckets[key]; !exists {
			missing++
		}
	}
	if len(s.loginBuckets)+missing > loginRateLimitMaxBuckets {
		return 60, false
	}
	for _, key := range keys {
		bucket := s.loginBuckets[key]
		if bucket.window.IsZero() || now.Sub(bucket.window) >= time.Minute {
			bucket = loginBucket{window: now}
		}
		if bucket.count >= 10 {
			remaining := int(time.Until(bucket.window.Add(time.Minute)).Seconds()) + 1
			return remaining, false
		}
		bucket.count++
		s.loginBuckets[key] = bucket
	}
	return 0, true
}

func (s *Server) loginClientIP(r *http.Request) string {
	remoteHost := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteHost = parsed
	}
	remote, err := netip.ParseAddr(remoteHost)
	if err != nil {
		return remoteHost
	}
	remote = remote.Unmap()
	state := s.runtime.Load()
	trusted := func(ip netip.Addr) bool {
		if state == nil {
			return false
		}
		for _, prefix := range state.config.TrustedProxies {
			if prefix.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(remote) {
		return remote.String()
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	client := remote
	for i := len(forwarded) - 1; i >= 0; i-- {
		if !trusted(client) {
			break
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(forwarded[i]))
		if err != nil {
			return remote.String()
		}
		client = ip.Unmap()
	}
	return client.String()
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil || memory < 8*1024 || memory > 256*1024 || iterations == 0 || iterations > 10 || parallelism == 0 || parallelism > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomID() string { return randomSecret(12) }

func constantEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func usernameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return usernameFromLogin(local)
}

// auditText makes text supplied by an unauthenticated client safe to store in an
// audit row: control characters (a NUL would make PostgreSQL refuse the row)
// are dropped and the length is capped, so failed sign-ins cannot fill the table
// with arbitrarily large values.
func auditText(value string) string {
	var b strings.Builder
	count := 0
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			continue
		}
		if count++; count > 128 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
