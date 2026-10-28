package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/keyring"
)

type managedSettings struct {
	PublicURL              string        `json:"public_url"`
	HFBaseURL              string        `json:"hf_base_url"`
	HFProxyURL             *string       `json:"hf_proxy_url,omitempty"`
	HFToken                string        `json:"hf_token"`
	HFAllowedHosts         []string      `json:"hf_allowed_hosts"`
	HFTimeout              time.Duration `json:"hf_timeout"`
	S3Endpoint             string        `json:"s3_endpoint"`
	S3Region               string        `json:"s3_region"`
	S3Bucket               string        `json:"s3_bucket"`
	S3Prefix               string        `json:"s3_prefix"`
	S3ForcePathStyle       bool          `json:"s3_force_path_style"`
	S3AccessKey            string        `json:"s3_access_key"`
	S3SecretKey            string        `json:"s3_secret_key"`
	S3ServerSideEncryption *bool         `json:"s3_server_side_encryption"`
	AuthMode               string        `json:"auth_mode"`
	AnonymousPublicRead    bool          `json:"anonymous_public_read"`
	SessionIdleTTL         time.Duration `json:"session_idle_ttl"`
	SessionAbsoluteTTL     time.Duration `json:"session_absolute_ttl"`
	CacheMaxFileSize       int64         `json:"cache_max_file_size"`
	CacheMaxTotalSize      *int64        `json:"cache_max_total_size,omitempty"`
	CacheRefTTL            time.Duration `json:"cache_ref_ttl"`
	CacheNegativeTTL       time.Duration `json:"cache_negative_ttl"`
	CacheGCGrace           time.Duration `json:"cache_gc_grace"`
	OIDCIssuer             string        `json:"oidc_issuer"`
	OIDCClientID           string        `json:"oidc_client_id"`
	OIDCClientSecret       string        `json:"oidc_client_secret"`
	OIDCRedirectURL        string        `json:"oidc_redirect_url"`
	OIDCAutoProvision      bool          `json:"oidc_auto_provision"`
	LDAPURL                string        `json:"ldap_url"`
	LDAPStartTLS           bool          `json:"ldap_starttls"`
	LDAPBindDN             string        `json:"ldap_bind_dn"`
	LDAPBindPassword       string        `json:"ldap_bind_password"`
	LDAPBaseDN             string        `json:"ldap_base_dn"`
	LDAPUserFilter         string        `json:"ldap_user_filter"`
	LDAPAutoProvision      bool          `json:"ldap_auto_provision"`
	MetricsEnabled         bool          `json:"metrics_enabled"`
	MetricsPath            string        `json:"metrics_path"`
	TracingEnabled         bool          `json:"tracing_enabled"`
	TracingServiceName     string        `json:"tracing_service_name"`
	TracingSamplingRatio   float64       `json:"tracing_sampling_ratio"`
	OTLPEndpoint           string        `json:"otlp_endpoint"`
	CacheInventoryRefresh  time.Duration `json:"cache_inventory_refresh"`
}

func LoadPersisted(ctx context.Context, sqlDB *sql.DB, base Config) (Config, error) {
	var ciphertext []byte
	err := sqlDB.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime'`).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return base, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("load saved settings: %w", err)
	}
	plaintext, _, err := secretRing(base).Open(ciphertext)
	if err != nil {
		return Config{}, fmt.Errorf("saved settings cannot be decrypted; check HFPROXY_SESSION_SECRET and HFPROXY_SESSION_SECRET_PREVIOUS")
	}
	var saved managedSettings
	if err := json.Unmarshal(plaintext, &saved); err != nil {
		return Config{}, fmt.Errorf("saved settings are invalid")
	}
	return saved.apply(base), nil
}

func SavePersisted(ctx context.Context, sqlDB *sql.DB, cfg Config, actorID string) error {
	plaintext, err := json.Marshal(managedFromConfig(cfg))
	if err != nil {
		return fmt.Errorf("encode settings")
	}
	ciphertext, err := secretRing(cfg).Seal(plaintext)
	if err != nil {
		return fmt.Errorf("encrypt settings")
	}
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO app_settings(setting_key,ciphertext,updated_by,updated_at)
		VALUES('runtime',$1,NULLIF($2,''),now()) ON CONFLICT(setting_key) DO UPDATE SET ciphertext=EXCLUDED.ciphertext,updated_by=EXCLUDED.updated_by,updated_at=now()`, ciphertext, actorID)
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}

func secretRing(cfg Config) *keyring.Ring {
	return keyring.NewRing(cfg.SessionSecret, cfg.PreviousSessionSecrets...)
}

// ReencryptPersisted seals the saved settings again under the current session
// secret. It reports whether anything had to change; settings already sealed
// under the current secret are left alone, and none saved is not an error.
func ReencryptPersisted(ctx context.Context, sqlDB *sql.DB, cfg Config) (bool, error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("start settings re-encryption: %w", err)
	}
	defer tx.Rollback()
	var ciphertext []byte
	err = tx.QueryRowContext(ctx, `SELECT ciphertext FROM app_settings WHERE setting_key='runtime' FOR UPDATE`).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load saved settings: %w", err)
	}
	ring := secretRing(cfg)
	plaintext, current, err := ring.Open(ciphertext)
	if err != nil {
		return false, fmt.Errorf("saved settings cannot be decrypted; check HFPROXY_SESSION_SECRET and HFPROXY_SESSION_SECRET_PREVIOUS")
	}
	if current {
		return false, nil
	}
	sealed, err := ring.Seal(plaintext)
	if err != nil {
		return false, fmt.Errorf("encrypt settings")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE app_settings SET ciphertext=$1,updated_at=now() WHERE setting_key='runtime'`, sealed); err != nil {
		return false, fmt.Errorf("save settings: %w", err)
	}
	return true, tx.Commit()
}

func DeletePersisted(ctx context.Context, sqlDB *sql.DB) error {
	_, err := sqlDB.ExecContext(ctx, `DELETE FROM app_settings WHERE setting_key='runtime'`)
	if err != nil {
		return fmt.Errorf("reset saved settings: %w", err)
	}
	return nil
}

func managedFromConfig(c Config) managedSettings {
	return managedSettings{
		PublicURL: c.PublicURL, HFBaseURL: c.HFBaseURL, HFProxyURL: stringPointer(c.HFProxyURL), HFToken: c.HFToken, HFAllowedHosts: c.HFAllowedHosts, HFTimeout: c.HFTimeout,
		S3Endpoint: c.S3Endpoint, S3Region: c.S3Region, S3Bucket: c.S3Bucket, S3Prefix: c.S3Prefix, S3ForcePathStyle: c.S3ForcePathStyle,
		S3AccessKey: c.S3AccessKey, S3SecretKey: c.S3SecretKey, S3ServerSideEncryption: boolPointer(c.S3ServerSideEncryption), AuthMode: c.AuthMode, AnonymousPublicRead: c.AnonymousPublicRead,
		SessionIdleTTL: c.SessionIdleTTL, SessionAbsoluteTTL: c.SessionAbsoluteTTL, CacheMaxFileSize: c.CacheMaxFileSize, CacheMaxTotalSize: int64Pointer(c.CacheMaxTotalSize),
		CacheRefTTL: c.CacheRefTTL, CacheNegativeTTL: c.CacheNegativeTTL, CacheGCGrace: c.CacheGCGrace,
		OIDCIssuer: c.OIDCIssuer, OIDCClientID: c.OIDCClientID, OIDCClientSecret: c.OIDCClientSecret, OIDCRedirectURL: c.OIDCRedirectURL,
		OIDCAutoProvision: c.OIDCAutoProvision, LDAPURL: c.LDAPURL, LDAPStartTLS: c.LDAPStartTLS, LDAPBindDN: c.LDAPBindDN,
		LDAPBindPassword: c.LDAPBindPassword, LDAPBaseDN: c.LDAPBaseDN, LDAPUserFilter: c.LDAPUserFilter, LDAPAutoProvision: c.LDAPAutoProvision,
		MetricsEnabled: c.MetricsEnabled, MetricsPath: c.MetricsPath, TracingEnabled: c.TracingEnabled, TracingServiceName: c.TracingServiceName, TracingSamplingRatio: c.TracingSamplingRatio, OTLPEndpoint: c.OTLPEndpoint, CacheInventoryRefresh: c.CacheInventoryRefresh,
	}
}

func (s managedSettings) apply(c Config) Config {
	c.PublicURL, c.HFBaseURL, c.HFToken, c.HFAllowedHosts, c.HFTimeout = s.PublicURL, s.HFBaseURL, s.HFToken, s.HFAllowedHosts, s.HFTimeout
	if s.HFProxyURL != nil {
		c.HFProxyURL = *s.HFProxyURL
	}
	c.S3Endpoint, c.S3Region, c.S3Bucket, c.S3Prefix, c.S3ForcePathStyle = s.S3Endpoint, s.S3Region, s.S3Bucket, s.S3Prefix, s.S3ForcePathStyle
	c.S3AccessKey, c.S3SecretKey, c.AuthMode, c.AnonymousPublicRead = s.S3AccessKey, s.S3SecretKey, s.AuthMode, s.AnonymousPublicRead
	if s.S3ServerSideEncryption != nil {
		c.S3ServerSideEncryption = *s.S3ServerSideEncryption
	}
	c.SessionIdleTTL, c.SessionAbsoluteTTL, c.CacheMaxFileSize = s.SessionIdleTTL, s.SessionAbsoluteTTL, s.CacheMaxFileSize
	// Settings saved before this limit existed leave it unset, so the value from
	// the environment is kept rather than reset to "unlimited".
	if s.CacheMaxTotalSize != nil {
		c.CacheMaxTotalSize = *s.CacheMaxTotalSize
	}
	c.CacheRefTTL, c.CacheNegativeTTL, c.CacheGCGrace = s.CacheRefTTL, s.CacheNegativeTTL, s.CacheGCGrace
	c.OIDCIssuer, c.OIDCClientID, c.OIDCClientSecret, c.OIDCRedirectURL = s.OIDCIssuer, s.OIDCClientID, s.OIDCClientSecret, s.OIDCRedirectURL
	c.OIDCAutoProvision, c.LDAPURL, c.LDAPStartTLS = s.OIDCAutoProvision, s.LDAPURL, s.LDAPStartTLS
	c.LDAPBindDN, c.LDAPBindPassword, c.LDAPBaseDN, c.LDAPUserFilter, c.LDAPAutoProvision = s.LDAPBindDN, s.LDAPBindPassword, s.LDAPBaseDN, s.LDAPUserFilter, s.LDAPAutoProvision
	c.MetricsEnabled, c.MetricsPath = s.MetricsEnabled, s.MetricsPath
	c.TracingEnabled, c.TracingServiceName, c.TracingSamplingRatio, c.OTLPEndpoint = s.TracingEnabled, s.TracingServiceName, s.TracingSamplingRatio, s.OTLPEndpoint
	if s.CacheInventoryRefresh > 0 {
		c.CacheInventoryRefresh = s.CacheInventoryRefresh
	}
	return c
}

// OverriddenFields lists the settings whose effective value differs from the
// one read from the environment because saved settings took precedence. Names
// only: values may be secrets.
func OverriddenFields(fromEnvironment, effective Config) []string {
	before, after := reflect.ValueOf(fromEnvironment), reflect.ValueOf(effective)
	var names []string
	for i := 0; i < before.NumField(); i++ {
		if !reflect.DeepEqual(before.Field(i).Interface(), after.Field(i).Interface()) {
			names = append(names, before.Type().Field(i).Name)
		}
	}
	return names
}

func boolPointer(value bool) *bool       { return &value }
func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }

// SettingSource describes where one setting's effective value comes from.
type SettingSource struct {
	Name   string
	Source string // "environment" or "console" (saved settings)
	Value  string // redacted: secrets are never shown
}

// DescribeSources reports, for every setting, whether the effective value
// comes from the environment or from settings saved in the console.
func DescribeSources(fromEnvironment, effective Config) []SettingSource {
	overridden := map[string]bool{}
	for _, name := range OverriddenFields(fromEnvironment, effective) {
		overridden[name] = true
	}
	v, t := reflect.ValueOf(effective), reflect.TypeOf(effective)
	out := make([]SettingSource, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		source := "environment"
		if overridden[name] {
			source = "console"
		}
		out = append(out, SettingSource{Name: name, Source: source, Value: redactedValue(name, v.Field(i).Interface())})
	}
	return out
}

func redactedValue(name string, value any) string {
	text := fmt.Sprint(value)
	switch value := value.(type) {
	case string:
		text = value
	case []string:
		text = strings.Join(value, ",")
	}
	for _, marker := range []string{"Secret", "Token", "Password", "AccessKey"} {
		if strings.Contains(name, marker) {
			if text == "" || text == "[]" {
				return "(unset)"
			}
			return "(set, hidden)"
		}
	}
	if name == "DatabaseURL" || name == "HFProxyURL" || name == "OTLPEndpoint" {
		if parsed, err := url.Parse(text); err == nil && parsed.User != nil {
			parsed.User = url.User("***")
			return parsed.String()
		}
	}
	if text == "" {
		return "(unset)"
	}
	return text
}
