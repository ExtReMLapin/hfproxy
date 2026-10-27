# Spécifications fonctionnelles et techniques — hfproxy

**Statut :** document de spécification, le produit n’est pas encore implémenté  
**Langage cible :** Go  
**Public :** Codex et développeurs chargés de réaliser le projet  
**Version :** 1.0

## 1. Objectif

Construire `hfproxy`, un proxy HTTP interne compatible avec les principaux flux de téléchargement de Hugging Face Hub. Les clients (notamment `huggingface_hub`, Transformers, Diffusers et les outils qui acceptent une URL de Hub personnalisée) interrogent hfproxy. Celui-ci sert une copie locale ou objet déjà en cache ; sinon, il télécharge le contenu depuis le Hub, le place dans un stockage objet S3 compatible (AWS S3 ou Garage), puis le sert au client.

Le produit comporte un serveur, une CLI d’administration et d’exploitation, et un petit dashboard web. L’identité des utilisateurs est gérée localement via OIDC/SSO ; LDAP/Active Directory est un fournisseur optionnel. Les permissions sont appliquées par hfproxy même lorsque l’accès à un objet est techniquement possible dans S3.

### Objectifs

- Réduire les téléchargements redondants depuis Hugging Face et fournir une URL stable interne.
- Cache persistant S3/Garage avec métadonnées PostgreSQL et déduplication des blobs par contenu.
- Supporter les dépôts `models`, `datasets` et `spaces` pour les opérations de lecture ciblées.
- Contrôler l’accès aux dépôts privés et gated au moyen d’un jeton HF serveur, sans exposer ce jeton aux clients.
- Donner aux administrateurs les moyens de gérer utilisateurs, rôles, dépôts, cache, configuration des fournisseurs et audit.
- Fournir un déploiement reproductible avec Docker Compose pour le développement et des images de conteneurs pour l’exploitation.

### Non-objectifs du MVP

- Émuler toute l’API Hugging Face Hub, les opérations de création/écriture de dépôts, l’upload Git/LFS, les webhooks, l’inférence et les Spaces exécutables.
- Implémenter le protocole de stockage Xet/CAS ou prétendre en être un endpoint. Le proxy traite le flux HTTP de résolution de fichiers renvoyé par le Hub.
- Répliquer l’interface publique huggingface.co ou fournir un miroir Git complet.
- Faire de LDAP un mécanisme de fédération OIDC : LDAP/AD est un fournisseur d’identité alternatif pour l’authentification locale. OIDC est privilégié pour SSO.
- Distribuer ou contourner les conditions d’accès/licences HF. L’identité technique HF utilisée en amont doit déjà avoir accès aux artefacts demandés.

## 2. Principes de conception

1. **Cache immuable par révision et chemin.** Un artefact correspond à un type de dépôt, un identifiant normalisé, une révision résolue (SHA de commit) et un chemin de fichier. Une branche est résolue à nouveau via le Hub ; elle ne doit jamais être considérée comme immuable.
2. **Objets de contenu adressés par hash.** Le corps téléchargé est écrit dans une zone temporaire puis publié sous une clé dérivée de SHA-256. Une entrée de métadonnées associe le fichier du dépôt à cet objet. Deux chemins identiques en contenu peuvent partager le même objet.
3. **PostgreSQL est la source de vérité des métadonnées.** S3 stocke uniquement les octets des fichiers ; ne pas lister le bucket pour reconstruire le catalogue lors du fonctionnement normal.
4. **Publication atomique.** Un fichier n’est visible comme cache hit qu’après validation complète, calcul de taille/hash, écriture réussie de l’objet et commit des métadonnées.
5. **Coalescence des téléchargements.** Une seule récupération amont en cours par clé d’artefact ; les requêtes concurrentes attendent le même résultat avec un timeout et une annulation corrects.
6. **Fermeture sécurisée par défaut.** Auth activée par défaut en production ; aucun endpoint de fichiers n’est accessible anonymement sauf configuration explicite d’une instance de lecture publique.
7. **Pas de jetons HF côté navigateur.** Le jeton amont réside dans le secret du serveur et n’est jamais renvoyé, journalisé ni stocké en clair en base.

## 3. Utilisateurs et autorisations

### Rôles intégrés

- `admin` : configuration, gestion des identités et rôles, politiques, purge, audit, accès au cache et à tous les dépôts.
- `operator` : visualisation de l’état, des dépôts et des téléchargements ; purge ciblée et rafraîchissement du cache selon les droits configurés ; pas de gestion des identités ni des secrets.
- `reader` : lecture des dépôts autorisés et consultation du dashboard non sensible.

Les rôles sont attribués à l’utilisateur local. Le modèle doit permettre d’ajouter ultérieurement des groupes et des permissions fines, sans les rendre nécessaires au MVP.

### Contrôles d’accès aux dépôts

- Politique de dépôt : `public`, `allowlist` ou `admin_only`.
- Les allowlists peuvent viser un utilisateur ou un groupe local. Refuser par défaut les dépôts privés/gated non autorisés.
- Chaque lecture de fichier vérifie l’identité, la politique du dépôt et l’état du compte avant de consulter le cache. Un cache hit ne doit jamais contourner l’autorisation.
- Un mode d’instance `anonymous_public_read` facultatif ne donne accès qu’aux dépôts explicitement marqués publics par un administrateur.
- L’identité HF amont est une identité de service. Les droits locaux constituent la barrière de sécurité pour les artefacts récupérés avec cette identité.
- Documenter clairement que le proxy n’est pas un mécanisme de transmission des demandes gated : l’utilisateur du token HF doit avoir déjà accepté les conditions et obtenu les autorisations upstream.

## 4. Architecture cible

Un binaire Go unique, `hfproxy`, avec sous-commandes `serve`, `migrate`, `admin`, `cache`, `config` et `version`. Le serveur expose les API HF compatibles et l’API/dashboard hfproxy.

### Composants

- **HTTP gateway** : routage, authentification, autorisation, limitation de débit, traduction des erreurs et compatibilité des en-têtes HTTP.
- **Hub client** : client HTTP sortant vers `HF_BASE_URL` (défaut `https://huggingface.co`), avec bearer token, timeouts, redirections contrôlées et respect des réponses Hub.
- **Cache service** : résolution de révision, coalescence, téléchargement temporaire, calcul d’empreinte, accès objet et invalidation.
- **Storage interface** : interface Go minimale (`Put`, `Get`, `Head`, `Delete`, `Presign` si activé) ; implémentation S3 via SDK Go officiel et compatibilité avec Garage. Les URL présignées sont désactivées par défaut afin de garder le contrôle d’accès dans hfproxy.
- **Repository** : PostgreSQL, migrations versionnées, transactions pour les métadonnées et verrous de coordination distribués en base.
- **Identity** : sessions web sécurisées, OIDC Authorization Code avec PKCE, et LDAP/AD optionnel avec bind TLS.
- **Dashboard** : ressources statiques servies par le binaire, interface responsive en français/anglais si le coût est raisonnable ; le serveur reste utilisable sans JavaScript pour les fonctions critiques d’administration CLI.
- **Audit/observabilité** : logs structurés sans secrets, métriques Prometheus optionnelles, endpoints de santé.

### Décisions de réalisation

- Utiliser la bibliothèque standard Go pour HTTP (`net/http`) et un routeur maintenu compatible avec le versionnement Go choisi ; éviter un framework imposant une architecture opaque.
- Séparer les paquets `cmd`, `internal/config`, `internal/http`, `internal/hub`, `internal/cache`, `internal/storage`, `internal/db`, `internal/auth`, `internal/rbac`, `internal/audit` et `web`.
- Dépendances à privilégier : pilote PostgreSQL maintenu (`pgx`), migrations versionnées, client OIDC maintenu et SDK AWS SDK for Go v2. Verrouiller les versions et les mettre à jour explicitement.
- L’interface ne doit pas devenir un SPA volumineux. Pages server-rendered ou petits assets statiques compilés ; aucun secret ne doit être rendu dans le HTML.
- Configuration prioritaire : fichier YAML optionnel, puis variables d’environnement, puis valeurs par défaut sûres. Les secrets ne sont jamais affichés par `config show`.

## 5. Compatibilité Hugging Face Hub

### Contrat pris en charge pour le MVP

Les clients configurés avec `HF_ENDPOINT=https://<hfproxy>` doivent pouvoir :

- résoudre et télécharger des fichiers de `models`, `datasets` et `spaces` via les routes `/resolve/` ;
- récupérer les métadonnées de dépôt et les listes de fichiers par les endpoints de lecture utilisés par les clients HF courants ;
- utiliser les révisions `main`, branches, tags et SHA de commit, les chemins URL-encodés, et les dépôts sous organisation ;
- reprendre une réponse partielle lorsque le backend amont et le client négocient correctement les requêtes `Range` ;
- recevoir les redirections nécessaires pour les fichiers volumineux sans que le navigateur soit exposé au token HF.

Les versions de `huggingface_hub`/Transformers prises en charge doivent être déclarées dans la documentation et épinglées dans les tests d’intégration. Le contrat porte sur les routes réellement utilisées et non sur la totalité des comportements privés du Hub.

### Routes compatibles côté proxy

Le routeur conserve les préfixes et méthodes Hub suivants. `{repo_id}` peut contenir `/` (ex. `org/model`) ; les segments de chemin doivent être décodés une seule fois et validés contre les traversées.

| Méthode | Route | Comportement MVP |
|---|---|---|
| `GET`, `HEAD` | `/{repo_type}/{repo_id}/resolve/{revision}/{filepath...}` | Route principale. `repo_type` vaut `models`, `datasets` ou `spaces`. Résoudre la révision, appliquer RBAC, servir cache ou récupérer l’amont. `HEAD` renvoie les métadonnées sans transférer le corps. |
| `GET` | `/api/models/{repo_id}` | Proxy de lecture vers l’API Hub ; filtrer les champs sensibles si nécessaire, contrôler l’accès avant appel. |
| `GET` | `/api/datasets/{repo_id}` | Idem pour dataset. |
| `GET` | `/api/spaces/{repo_id}` | Idem pour Space (métadonnées uniquement ; aucune exécution). |
| `GET` | `/api/{repo_type}/{repo_id}/revision/{revision}` | Métadonnées de révision si utilisées par le client cible ; préserver status, headers et JSON compatibles. |
| `GET` | `/api/{repo_type}/{repo_id}/tree/{revision}[/{path}]` | Liste de fichiers/répertoires selon le contrat Hub ; pagination et paramètres de requête préservés. |
| `GET` | `/api/{repo_type}/{repo_id}/commits/{revision}` | Métadonnées des commits, si requis par la matrice d’intégration. |
| `GET` | `/api/whoami-v2` | Retourne l’identité hfproxy authentifiée sous un schéma documenté, ou un statut compatible non authentifié ; ne pas prétendre à l’identité du compte HF amont. |
| `GET` | `/api/health` | Santé du Hub et du service uniquement si cette route est exigée par un client ; ne pas exposer des détails d’infrastructure. |

Les routes API Hub non listées sont hors MVP et répondent `404` ou `501` avec une erreur JSON claire. Ne pas implémenter un proxy générique de toute URL utilisateur : cela créerait un risque SSRF.

### Traitement de `/resolve/`

1. Authentifier le client selon la configuration ; extraire un jeton hfproxy de `Authorization: Bearer …` si le mode jeton est activé. Les jetons hfproxy sont indépendants des tokens `hf_…` amont.
2. Normaliser le type, l’identifiant de dépôt, la révision et le chemin ; refuser les segments `.`/`..`, NUL, séparateurs encodés ambigus, caractères de contrôle et chemins au-delà des limites configurées.
3. Vérifier la permission locale avant tout hit cache ou appel amont.
4. Résoudre la référence de révision vers un SHA immutable via l’endpoint officiel de résolution/metadata du Hub. Une réponse amont `X-Repo-Commit` ou équivalent, si présente, est préférée ; ne pas déduire un SHA d’un nom de branche.
5. Sur cache hit, répondre en streaming depuis l’objet S3, définir les en-têtes de contenu cohérents et prendre en charge `HEAD`, `If-None-Match`, `If-Modified-Since` et `Range`/`If-Range` lorsque les métadonnées permettent une réponse correcte.
6. Sur miss, acquérir le verrou de clé, recontrôler le cache, puis télécharger depuis l’URL Hub autorisée. Ne jamais suivre une redirection vers un hôte arbitraire. Pour les redirections signées vers CDN/object storage HF, suivre uniquement des hôtes configurés/validés, supprimer le bearer HF après redirection hors domaine de confiance et ne jamais recopier l’URL signée dans les logs.
7. Écrire dans un objet temporaire non publiquement accessible ; contrôler la taille maximum, le timeout, le nombre de redirections et l’intégrité si un digest fiable est fourni. Calculer SHA-256 en flux.
8. Publier l’objet final (écriture S3 puis transaction DB). Si l’enregistrement échoue, l’objet doit être nettoyable par le job de réconciliation ; ne pas exposer un cache hit incomplet.
9. Envoyer le corps au client ou le relire depuis l’objet publié. Retourner les en-têtes upstream utiles (`ETag`, `Last-Modified`, `Content-Length`, `Content-Type`, `Accept-Ranges`, `Content-Range`, `Cache-Control`) en supprimant cookies, identifiants, headers hop-by-hop et toute information sensible.

### Règles de compatibilité HTTP

- Préserver les méthodes et paramètres de requête documentés ; ne pas transférer les headers `Host`, `Connection`, `Transfer-Encoding`, `TE`, `Trailer`, `Upgrade` ou `Proxy-*`.
- Le client peut envoyer un token hfproxy via `Authorization`. Ne jamais transmettre ce token au Hub. Ajouter le token HF serveur uniquement sur les requêtes amont autorisées.
- Mapper les erreurs upstream sans transformer `401/403` en cache hit ou en `404`. Conserver les statuts utiles (`400`, `401`, `403`, `404`, `410`, `429`, `5xx`) avec un corps JSON compatible pour API et réponse HTTP adaptée pour resolve.
- Retourner `429` local avec `Retry-After` pour les limites locales. Respecter et borner les indications de retry upstream.
- Ne pas mettre en cache durablement les erreurs d’authentification, de permission, `429` ou `5xx`. Un `404` peut être brièvement mémorisé avec TTL faible configurable, sans confondre dépôt privé inaccessible et ressource inexistante.
- Le transfert de gros fichiers est en streaming, sans charger le fichier entier en RAM. Les tailles et délais sont configurables ; valeur initiale de taille maximale : 100 GiB, révisable avant version stable.
- Pour les fichiers privés/gated, toute URL de redirection signée est à usage temporaire. Le proxy ne doit pas retourner un lien présigné S3 à un utilisateur non autorisé.

### Limites HF explicites

- Le Hub utilise plusieurs mécanismes de livraison, notamment des redirections et Xet pour certains dépôts. hfproxy doit traiter le flux HTTP observable, conserver les headers de résolution nécessaires et ne pas supposer que tous les blobs sont servis depuis un même hôte.
- Xet/CAS natif, le téléchargement partiel par chunks Xet et les routes annexes non documentées ne sont pas garantis. Si un client tente un protocole non pris en charge, renvoyer une erreur diagnostiquable et documenter le mode de contournement temporaire via endpoint HF direct.
- La compatibilité doit être pilotée par des tests avec versions épinglées de `huggingface_hub` et Transformers, sur dépôts publics de test. Les tests privés/gated nécessitent des credentials d’intégration fournis par l’opérateur et sont désactivés en CI publique.

## 6. API hfproxy

Toutes les routes ci-dessous sont préfixées `/api/v1`. Les formats d’erreur sont `application/problem+json` avec `type`, `title`, `status`, `detail`, `request_id`. Pagination par `limit` (défaut 50, maximum 200) et `cursor` opaque.

### Session et identité

| Méthode / route | Fonction |
|---|---|
| `GET /auth/login` | Démarre le flux OIDC (ou affiche les fournisseurs configurés). |
| `GET /auth/callback` | Callback OIDC avec `state`, nonce et PKCE vérifiés ; crée une session locale. |
| `POST /auth/logout` | Invalide la session serveur et efface le cookie. CSRF protégé. |
| `GET /me` | Profil local, rôles et groupes visibles. |
| `GET /auth/providers` | Liste publique des modes de connexion actifs, sans secret ni URL sensible. |

### Administration et exploitation

| Méthode / route | Fonction |
|---|---|
| `GET /healthz` | Liveness sans dépendance externe. |
| `GET /readyz` | Readiness : DB, migrations et stockage accessibles ; aucune information sensible dans le corps. |
| `GET /metrics` | Prometheus, désactivé ou protégé par défaut ; jamais public par accident. |
| `GET /stats` | Volume en cache, hit ratio, taux/erreurs, taille objet agrégée. |
| `GET /repos` / `POST /repos` | Liste et enregistrement d’une politique de dépôt (`repo_type`, `repo_id`, `access_policy`). |
| `GET /repos/{id}` / `PATCH /repos/{id}` / `DELETE /repos/{id}` | Détail et politique. Suppression logique par défaut ; purge séparée. |
| `GET /cache/entries` | Recherche paginée par dépôt, révision, chemin, taille et date. |
| `POST /cache/entries/{id}/purge` | Purge d’une entrée, avec vérification de références blob et audit. |
| `POST /cache/purge` | Purge filtrée avec simulation (`dry_run=true`) et confirmation explicite ; admin uniquement. |
| `GET /users` / `PATCH /users/{id}` | Gestion utilisateur local, désactivation et rôles. Création locale désactivée par défaut si OIDC/LDAP est actif. |
| `GET /groups` / `POST /groups` / `PATCH /groups/{id}` | Groupes et membres locaux, MVP simple. |
| `GET /audit-events` | Consultation paginée des événements selon permissions. |
| `GET /settings/public` | Paramètres non secrets et état des fonctionnalités. |

Les mutations administratives sont `admin` sauf permissions spécifiques d’`operator`. Les API ne retournent jamais secret, token, mot de passe, cookie ou URL présignée. Les endpoints de configuration des secrets ne doivent pas permettre de relire la valeur après écriture.

### API CLI

`hfproxy` est le nom du binaire ; l’aide doit être disponible avec `hfproxy --help` et chaque sous-commande.

| Commande | Comportement |
|---|---|
| `hfproxy serve` | Démarre le serveur. Écoute sur loopback par défaut en mode local ; conteneur configurable. Arrêt propre sur SIGTERM/SIGINT. |
| `hfproxy migrate up` | Applique les migrations ; jamais implicitement en production sauf option explicite au démarrage. |
| `hfproxy migrate status` | Affiche la version DB sans secrets. |
| `hfproxy admin create-user` | Crée le premier admin local, mot de passe demandé sans écho ou fourni par canal secret ; hash Argon2id. Refuse si un admin existe. |
| `hfproxy admin reset-password` | Remplace le mot de passe Argon2id d’un admin local existant via fichier/canal secret et révoque ses sessions web. |
| `hfproxy admin token create|revoke|list` | Gère les tokens clients hfproxy, affichés une seule fois à la création, hashés en base, scope/expiration optionnels. |
| `hfproxy cache inspect|purge|verify` | Opérations cache ; purge avec simulation par défaut et confirmation requise. |
| `hfproxy config validate` | Valide la configuration et signale les erreurs sans imprimer les valeurs secrètes. |
| `hfproxy version` | Version, commit et version Go compilée. |

Les valeurs peuvent venir de flags non secrets, variables d’environnement ou fichier de config. Ne pas accepter les secrets directement en argument CLI, car ils peuvent apparaître dans l’historique/process list.

## 7. Authentification : OIDC/SSO et LDAP/AD

### OIDC — requis

- Authorization Code + PKCE (S256), `state`, nonce, vérification signature/issuer/audience/expiration et HTTPS obligatoire hors développement.
- Discovery OIDC configurable ; client ID non secret, client secret via secret d’environnement/fichier monté.
- Callback et URLs de redirection sur allowlist stricte ; pas d’open redirect.
- Association de compte stable par `(issuer, subject)` ; ne pas fusionner automatiquement sur la seule adresse e-mail. Option d’auto-provisionnement configurable, désactivée par défaut ou limitée à domaines/groupes autorisés.
- Mapping des claims vers nom affiché, email vérifié, groupes ; mapping groupes→rôles configuré explicitement. Refuser l’octroi implicite de `admin` depuis un claim générique.
- Sessions opaques en base (ou stockage partagé), cookie `Secure`, `HttpOnly`, `SameSite=Lax`, rotation d’identifiant après connexion, expiration idle et absolue configurables, révocation au logout.
- Protection CSRF pour mutations web, CSP stricte, `X-Content-Type-Options`, `Referrer-Policy` et politique anti-clickjacking.

### LDAP / Active Directory — optionnel

- Connexion LDAPv3 avec LDAPS ou StartTLS obligatoire par défaut ; option plain LDAP uniquement pour développement explicite.
- Bind utilisateur via DN template ou recherche base/filter configurables ; échapper correctement les filtres LDAP ; protéger contre injection LDAP.
- Recherche groupes optionnelle, mapping groupe→rôle en allowlist, pagination LDAP si nécessaire. Compte local d’urgence désactivable après première configuration.
- Timeout court, pool/reconnexion bornés, limitation des tentatives, délai progressif, messages d’erreur ne révélant pas l’existence d’un utilisateur.
- Ne pas stocker le mot de passe LDAP ; le transmettre seulement à l’annuaire via TLS et l’effacer au plus tôt de la mémoire.
- Tests d’intégration via conteneur OpenLDAP/AD de test ; l’intégration AD réelle dépend des attributs et règles de l’organisation.

### Tokens d’accès hfproxy

- Jeton aléatoire à entropie suffisante, préfixe distinctif, empreinte hashée côté serveur (HMAC/sha-256 avec pepper secret ou hash adapté), affichage unique.
- Enregistrement de création, dernier usage, expiration, révocation, scope et propriétaire ; réponse d’auth identifie le principal et applique RBAC.
- Comparaison en temps constant ; jamais de stockage en clair, de journalisation ou d’inclusion dans les erreurs.

## 8. Modèle de données PostgreSQL

Tables minimales, avec UUID, timestamps UTC, contraintes et index explicites :

- `users(id, username, email, display_name, status, created_at, updated_at)`.
- `external_identities(id, user_id, provider, issuer, subject, created_at)` avec unicité `(issuer, subject)`.
- `groups(id, name, source, created_at)` et `group_members(group_id, user_id)`.
- `roles(id, name)` et `user_roles(user_id, role_id)` ; mapping OIDC/LDAP stocké en configuration versionnée ou tables administratives selon décision d’implémentation.
- `access_tokens(id, user_id, token_hash, prefix, scopes, expires_at, revoked_at, last_used_at)`.
- `repositories(id, repo_type, repo_id, access_policy, created_by, created_at, updated_at)` unique sur `(repo_type, repo_id)`.
- `repo_grants(repository_id, subject_type, subject_id, permission)`.
- `revisions(id, repository_id, requested_ref, commit_sha, resolved_at)` ; index de cache TTL pour refs mobiles.
- `blobs(id, sha256, size_bytes, storage_key, content_type, created_at, verified_at)` unique sur SHA.
- `artifacts(id, repository_id, commit_sha, path, blob_id, etag, last_modified, upstream_size, cached_at, last_accessed_at)` unique sur `(repository_id, commit_sha, path)`.
- `downloads(id, repository_id, commit_sha, path, status, started_at, completed_at, bytes, error_class)` ; rétention configurable et champs sans secret.
- `audit_events(id, actor_user_id, action, target_type, target_id, metadata_json, request_id, created_at)` ; metadata filtrée et taille plafonnée.
- `schema_migrations` gérée par l’outil de migration.

Ne pas stocker le contenu des modèles, tokens HF upstream, mots de passe LDAP/OIDC ni cookies en base. Les champs `metadata_json` d’audit sont une liste d’attributs explicitement autorisés, jamais une copie de requête HTTP complète.

## 9. Stockage objet et stratégie cache

- Configurer endpoint, région, bucket, prefix, path-style, chiffrement, credentials via secret et CA TLS personnalisable. L’environnement local utilise Garage en mode path-style.
- Bucket privé ; désactiver ACL publiques ; chiffrement côté serveur conseillé. Fournir une politique IAM minimale pour lecture/écriture/suppression dans le préfixe hfproxy.
- Clé d’objet : `blobs/sha256/<2 premiers hex>/<sha256>` ; temporaires dans `staging/<request-id>` avec suppression par TTL. Ne jamais inclure directement un chemin utilisateur dans une clé S3.
- Téléchargement en streaming multipart si taille le nécessite ; annulation au départ du dernier demandeur selon politique ; multipart incomplet supprimé par nettoyage périodique.
- Métadonnées S3 utiles : hash, taille et version applicative, sans token ni chemin privé exposé publiquement.
- Garbage collection : identifier les blobs sans références en DB, attendre un délai de grâce configurable, puis supprimer ; opérations par lots et journalisées. `dry_run` obligatoire par défaut.
- Supporter les erreurs transitoires avec retry exponentiel, jitter, plafond et idempotence. Ne pas réessayer indéfiniment des erreurs de permission ou validation.
- Cache des résolutions de branche avec TTL court configurable (défaut 60 s) ; SHA de commit immutable. ETag/TTL et politique de rafraîchissement configurable ; commande de refresh manuelle pour un dépôt/révision.
- Métadonnées sensibles aux autorisations restent en DB ; un utilisateur ne peut pas accéder au même blob via un autre artefact sans autorisation propre.

## 10. Sécurité, confidentialité et abus

### Réseau et transport

- TLS au reverse proxy ou TLS natif configurable ; documenter le mode de terminaison et les headers `X-Forwarded-*` de confiance. N’honorer ces headers que depuis les proxies explicitement autorisés.
- Egress HTTP limité à l’hôte HF configuré et à ses hôtes de livraison connus/configurés ; DNS rebinding et redirects contrôlés ; pas de fetch d’URL fourni librement par le client.
- Connexions PostgreSQL et S3 chiffrées en production si supporté.
- Limites configurables : taille d’URL/header, corps des API, connexions simultanées, downloads en cours, durée de connexion, taille fichier, débit et nombre de requêtes par utilisateur/IP.

### Protection des secrets

- Jeton HF read-only et finement scoped ; injecté par variable d’environnement ou secret manager, jamais dans fichier exemple ni image.
- Masquer `Authorization`, `Cookie`, query parameters de signature et URL présignées dans logs/traces/errors.
- Redaction automatique de config et erreurs ; endpoint de diagnostic ne montre que disponibilité et noms logiques de dépendances.
- Rotation supportée sans redémarrage si possible, sinon procédure documentée de redémarrage.

### Web et données

- Argon2id pour comptes locaux, politique de mot de passe robuste, limitation brute-force, verrouillage progressif et aucun compte admin par défaut.
- Protection CSRF/XSS/SQL injection ; templates avec échappement, requêtes SQL paramétrées, CSP, cookies sûrs.
- Contrôle des chemins et encodages contre path traversal, confusion de route et header injection.
- Logs d’audit pour connexion, échec auth, attribution rôle, changement de policy, purge et téléchargements ; exclure tokens, mots de passe, contenu téléchargé.
- Rétention configurable pour logs DB ; procédure de sauvegarde/restauration PostgreSQL et bucket documentée ; test de restauration avant production.
- Dépendances analysées en CI, image minimale non-root, filesystem read-only lorsque possible, capabilities minimales, SBOM et provenance à ajouter avant production.

## 11. Dashboard web

Pages minimales :

1. **Connexion** : boutons OIDC et/ou formulaire LDAP/local selon providers actifs.
2. **Vue d’ensemble** : état DB/S3/Hub (OK/erreur), taille cache, nombre d’artefacts, hit ratio, téléchargements et erreurs récents.
3. **Dépôts** : recherche, type/id, politique, date d’activité, actions selon RBAC.
4. **Cache** : recherche par dépôt, chemin, SHA, taille ; détail entrée ; purge ciblée et purge simulée avant confirmation.
5. **Utilisateurs et groupes** : activation, rôle et mapping ; admin uniquement.
6. **Audit** : filtres période, acteur, action et cible.
7. **Paramètres** : état OIDC/LDAP/S3/Hub, jamais leurs secrets ; config modifiée via CLI ou écran protégé selon MVP.

Chaque action destructive affiche l’impact estimé et nécessite confirmation. Les changements sont audités. Les utilisateurs sans permission ne voient pas les actions désactivées ni les détails d’erreurs sensibles.

## 12. Observabilité et opérations

- Logs JSON sur stdout ; champs `timestamp`, `level`, `request_id`, `actor_id` (si connu), `route`, `status`, `duration_ms`, `cache_status`, `upstream_status`, `bytes`. Redaction obligatoire.
- Métriques : `http_requests_total`, `http_request_duration_seconds`, `cache_requests_total{result}`, `cache_bytes_total`, `upstream_requests_total{status}`, `downloads_in_progress`, `download_bytes_total`, `storage_operations_total{operation,status}`, `auth_failures_total`, `db_pool_in_use`.
- Ne pas inclure repo_id/path comme labels Prometheus à cardinalité élevée ; dimensions détaillées dans logs/audit avec règles d’accès.
- Liveness (`/healthz`) ne vérifie pas les dépendances ; readiness (`/readyz`) vérifie DB, migrations et S3. La santé upstream HF peut être un indicateur distinct sans rendre liveness rouge.
- Arrêt gracieux : cesser l’acceptation, laisser finir les réponses jusqu’au délai configurable, annuler ensuite téléchargements et fermer pools.
- Fournir des procédures : initialisation, sauvegarde, restauration, rotation token HF, rotation OIDC/LDAP/S3, purge, mise à niveau et retour arrière DB.

## 13. Configuration et variables

Nom de variable : `HFPROXY_...` ; accepter les secrets depuis un fichier monté avec suffixe `_FILE` pour les conteneurs.

Paramètres de référence (noms indicatifs, Codex peut les ajuster en conservant les sémantiques) :

- `HFPROXY_LISTEN_ADDR`, `HFPROXY_PUBLIC_URL`, `HFPROXY_LOG_LEVEL`, `HFPROXY_TRUSTED_PROXIES`.
- `HFPROXY_DATABASE_URL`, `HFPROXY_DATABASE_MAX_CONNS`, `HFPROXY_AUTO_MIGRATE`.
- `HFPROXY_HF_BASE_URL`, `HFPROXY_HF_TOKEN`, `HFPROXY_HF_ALLOWED_REDIRECT_HOSTS`, `HFPROXY_HF_TIMEOUT`.
- `HFPROXY_S3_ENDPOINT`, `HFPROXY_S3_REGION`, `HFPROXY_S3_BUCKET`, `HFPROXY_S3_PREFIX`, `HFPROXY_S3_FORCE_PATH_STYLE`, `HFPROXY_S3_ACCESS_KEY`, `HFPROXY_S3_SECRET_KEY`, `HFPROXY_S3_CA_FILE`.
- `HFPROXY_OIDC_ISSUER`, `HFPROXY_OIDC_CLIENT_ID`, `HFPROXY_OIDC_CLIENT_SECRET`, `HFPROXY_OIDC_REDIRECT_URL`, `HFPROXY_OIDC_ALLOWED_DOMAINS`, `HFPROXY_OIDC_GROUP_CLAIM`.
- `HFPROXY_LDAP_URL`, `HFPROXY_LDAP_STARTTLS`, `HFPROXY_LDAP_BIND_DN`, `HFPROXY_LDAP_BIND_PASSWORD`, `HFPROXY_LDAP_BASE_DN`, `HFPROXY_LDAP_USER_FILTER`, `HFPROXY_LDAP_GROUP_FILTER`.
- `HFPROXY_CACHE_MAX_FILE_SIZE`, `HFPROXY_CACHE_REF_TTL`, `HFPROXY_CACHE_NEGATIVE_TTL`, `HFPROXY_CACHE_GC_GRACE`.
- `HFPROXY_AUTH_MODE`, `HFPROXY_ANONYMOUS_PUBLIC_READ`, `HFPROXY_SESSION_SECRET`, `HFPROXY_SESSION_IDLE_TTL`, `HFPROXY_SESSION_ABSOLUTE_TTL`.

Au démarrage, valider les champs obligatoires et échouer avec un message actionnable sans afficher de valeur secrète. En production, refuser une session secret faible, une URL callback non TLS, un bucket public connu ou un mode sans authentification sauf opt-in explicite.

## 14. Plan de développement incrémental

Chaque étape doit livrer un logiciel utilisable et conserver les critères antérieurs.

### Étape 0 — squelette et décisions

- Initialiser module Go, structure des paquets, configuration, logs, erreurs, version, Dockerfile et README.
- Décider et consigner versions minimales Go et des dépendances ; ajouter conventions de formatage et CI.
- Livrer `hfproxy version`, `config validate`, serveur liveness.

### Étape 1 — proxy public et cache objet

- PostgreSQL migrations pour repos/révisions/blobs/artifacts/downloads.
- S3/Garage, client HF public sans token, `/resolve/` streaming, résolution SHA, coalescence, cache miss/hit, HEAD, ETag et Range.
- Premier test de bout en bout avec modèle public de petite taille, Garage et Postgres en Compose.

### Étape 2 — Auth et permissions

- Comptes locaux de bootstrap, sessions web, tokens hfproxy, rôles intégrés, policies de dépôt, audit.
- Vérifier qu’un cache hit privé ne contourne pas le contrôle d’accès.
- Protéger les endpoints d’administration et limiter les abus.

### Étape 3 — OIDC/SSO

- Code flow + PKCE, auto-provisionnement contrôlé, mappings de groupes, gestion de session et écrans de connexion.
- Fournir un fournisseur de test OIDC dans Compose et scénarios d’intégration.

### Étape 4 — API de métadonnées et dashboard

- Ajouter les endpoints de lecture Hub requis par la matrice HF épinglée.
- Dashboard vue d’ensemble, dépôts, cache, utilisateurs, groupes et audit.
- Documenter les incompatibilités connues et le paramétrage des clients.

### Étape 5 — LDAP/AD et durcissement

- LDAP/AD TLS, mapping groupes, protections brute-force et intégration OpenLDAP de test.
- Durcir egress/redirects, limites, headers web, rotation secrets, sauvegarde/restauration et image non-root.

### Étape 6 — production readiness

- Tests de charge sur plusieurs gros fichiers, téléchargements simultanés, reprise Range, concurrence multi-instance, GC et panne réseau.
- SBOM, scan dépendances/images, guide exploitation, upgrade/rollback, métriques et alertes.
- Décider de la compatibilité Xet selon preuves issues de tests réels ; ne pas annoncer support avant la suite correspondante.

## 15. Tests exigés

Les tests ci-dessous sont des critères de livraison et doivent être développés au cours des étapes correspondantes ; aucune suite de tests n’est incluse avec ce document de spécification.

- **Unitaires** : parsing/normalisation route et chemins, politique RBAC, mapping d’erreurs, configuration/redaction, token hash, résolveur de révision, calcul de hash et règles cache.
- **HTTP** : `/resolve/` GET/HEAD, ETag/304, Range/206, 416, erreurs Hub, en-têtes hop-by-hop, redirections et nettoyage des secrets.
- **Intégration** : PostgreSQL + Garage, migrations, publication atomique, reprise après échec S3/DB, GC sans suppression de blobs référencés.
- **Concurrence** : plusieurs demandes même clé ne produisent qu’un téléchargement upstream ; annulation et timeout libèrent le verrou ; tests `-race`.
- **Auth** : OIDC state/nonce/PKCE/issuer invalides, callback forgé, session expiry/rotation/logout, CSRF, LDAP injection, TLS requis, brute force.
- **RBAC** : lecteur non autorisé refusé avant accès cache ; operator ne peut changer rôles/secrets ; admin peut auditer/purger.
- **Compatibilité client** : script Python minimal avec versions fixées de `huggingface_hub` et Transformers utilisant `HF_ENDPOINT`, metadata, `hf_hub_download` et chargement d’un config/poids public.
- **Sécurité** : tests SSRF/redirection vers localhost, traversal encodé, injection d’en-tête, logs sans bearer/token/URL signée, dépassement de taille et requêtes lentes.
- **E2E Compose** : premier téléchargement, hit cache, restart du proxy sans perdre le cache, authentification OIDC, policy de dépôt et purge.
- **Exploitation** : image non-root, healthcheck, arrêt gracieux, config invalide sans fuite et sauvegarde/restauration documentée.

Les tests externes HF doivent utiliser un dépôt public dédié et stable. Aucun test CI ne doit dépendre d’un modèle massif, d’un token personnel ou d’un service HF non disponible sans signaler clairement qu’il est optionnel.

## 16. Docker et livraison

Le dépôt d’implémentation attendu après création par Codex doit inclure :

- `Dockerfile` multi-stage, image finale minimale, processus non-root, CA certifiées présentes, binaire statique si compatible ; aucun secret de build.
- `docker-compose.yml` de développement avec `hfproxy`, PostgreSQL, Garage en nœud unique et création du bucket initial. OIDC/LDAP de test peuvent être profils optionnels.
- `docker-compose.example.yml` ou `.env.example` sans secret réel, avec commentaires sur secrets, TLS, volumes, limites et bootstrap.
- Volumes persistants DB et Garage ; healthchecks et dépendances `service_healthy`.
- Le port HTTP interne ne doit pas être exposé publiquement dans un exemple production sans TLS/reverse proxy et authentification configurés.
- Documentation pour Kubernetes/Helm hors MVP, mais variables, probes, stockage persistant et SIGTERM doivent rendre cette évolution possible.

## 17. Critères d’acceptation

Le MVP est accepté lorsque tous les éléments suivants sont démontrés :

1. `docker compose up` démarre hfproxy, PostgreSQL et Garage avec des identifiants de développement ; la configuration de production injecte ses secrets explicitement.
2. Une requête `GET /models/{repo}/resolve/{revision}/{path}` sur dépôt public fait un seul téléchargement amont, enregistre l’objet et la métadonnée, puis retourne les octets identiques.
3. La seconde requête retourne les mêmes octets depuis Garage sans téléchargement HF supplémentaire ; les compteurs hit/miss sont exacts.
4. Les révisions de branche résolvent un commit SHA et ne mélangent pas les contenus de deux commits ; le TTL de branche est appliqué.
5. `HEAD`, ETag/304 et les requêtes Range sont correctes ou refusées explicitement si le comportement amont ne permet pas de les satisfaire ; aucune réponse 206 incorrecte.
6. Les appels à un dépôt privé/gated fonctionnent uniquement lorsque le token HF serveur y a accès et que l’utilisateur local est autorisé ; un utilisateur non autorisé est refusé même si l’objet est en cache.
7. OIDC fonctionne avec un IdP de test en Authorization Code + PKCE ; groupes configurés mappent les rôles ; logout invalide réellement la session.
8. LDAP/AD peut être activé sans changer le modèle de RBAC et une connexion LDAPS/StartTLS de test fonctionne ; il reste désactivé sans configuration.
9. La CLI permet validation config, migration, création du premier admin, version, inspect et purge simulée ; aucune commande ne divulgue les secrets.
10. Le dashboard applique RBAC, affiche le cache et l’état des services, permet les actions autorisées et inscrit les mutations dans l’audit.
11. Les routes et versions clients déclarées dans la matrice de compatibilité réussissent les tests d’intégration ; incompatibilités connues sont documentées.
12. Les logs, erreurs, métriques et audit ne contiennent aucun token HF, jeton utilisateur, mot de passe ou URL signée.
13. Les limites de taille, débit, redirection, timeout et concurrence sont appliquées ; les échecs amont n’entraînent ni publication partielle ni corruption du cache.
14. Le conteneur fonctionne non-root, répond aux probes, s’arrête proprement et reprend après redémarrage avec cache intact.
15. Guide d’installation, configuration HF_ENDPOINT, OIDC, LDAP, S3/Garage, exploitation, backup/restore, limites de compatibilité et dépannage sont présents.

## 18. Risques et décisions à confirmer pendant le projet

| Risque | Conséquence | Réponse attendue |
|---|---|---|
| Évolution du protocole Hub, des headers ou des CDN | Régressions client ou téléchargements incomplets | Tests de compatibilité épinglés, headers préservés prudemment, versionnement et documentation des limites. |
| Xet/CAS ou gros fichiers servis via des chemins spécialisés | Certains clients ou transferts ne passent pas par le flux `/resolve/` classique | Valider sur modèles réels ; ne pas revendiquer la compatibilité Xet sans tests ; autoriser un fallback contrôlé. |
| Token upstream mutualisé | Un utilisateur local pourrait accéder à des artefacts qu’il ne devrait pas voir | RBAC avant chaque lecture, politiques explicites, audit, tests cache hit et identité upstream minimale. |
| Licences et conditions gated | Une mise en cache peut faciliter une redistribution contraire aux conditions | Documentation, politique locale, journal d’accès, accès gated soumis aux mêmes contrôles ; l’opérateur assume la conformité. |
| CDN et URLs signées | Fuite de bearer ou URL temporaire | Allowlist d’hôtes, suppression du bearer après redirection externe, redaction de logs, pas de redirection S3 publique par défaut. |
| Grand nombre de fichiers/volumes | Saturation PostgreSQL, S3, métriques et dashboard | Pagination, index, labels métriques bornés, GC en lots, quotas par utilisateur/instance. |
| Disponibilité HF lors d’un cache miss | Échec de chargement de modèles non pré-cachés | Messages clairs, timeouts/retries bornés, préchauffage futur, statut upstream visible.
| LDAP hétérogène et vieilles configs AD | Erreurs de bind/mapping ou sécurité TLS faible | Paramètres explicitement testés, TLS par défaut, fixtures OpenLDAP, guide d’attributs spécifiques à l’annuaire. |
| Multi-instance et verrouillage | Téléchargements doublés ou états concurrents | Verrou distribué PostgreSQL avec lease/expiry, publication idempotente et tests concurrence multi-instance. |

Décisions à prendre avant la version stable : versions exactes de Go et clients HF prises en charge ; domaines CDN HF autorisés selon réponses observées ; taille maximale de fichier ; durées de session et de rétention ; policy par défaut sur nouveaux dépôts ; gestion des groupes depuis l’IdP ; besoin de URLs présignées S3 ou de téléchargement toujours relayé ; comportement de refresh des révisions mobiles.

## 19. Documentation de référence

Ces pages Hugging Face sont des références externes à revérifier lors de l’implémentation ; elles ne remplacent pas les tests de compatibilité du projet.

- [Hub API Endpoints et OpenAPI](https://huggingface.co/docs/hub/en/api) — documentation officielle indiquant que la référence complète est l’OpenAPI du Hub.
- [Hub rate limits](https://huggingface.co/docs/hub/main/en/rate-limits) — distingue notamment les routes Resolver `/resolve/` des API Hub.
- [User access tokens](https://huggingface.co/docs/hub/security-tokens) — scopes et bonnes pratiques pour les tokens upstream.
- [Gated models](https://huggingface.co/docs/hub/en/models-gated) — accès gated, approbation individuelle et authentification nécessaire pour les fichiers.
- [Hugging Face cache et Xet](https://huggingface.co/docs/huggingface_hub/main/guides/manage-cache) — décrit l’optimisation Xet et le cache chunks, que le MVP ne réimplémente pas.

---

**Instruction d’implémentation pour Codex :** commencer par l’étape 0, inspecter l’environnement et les versions disponibles, puis exécuter les étapes dans l’ordre. Avant chaque étape, actualiser les références officielles Hugging Face et les dépendances Go maintenues. Ne pas déclarer une fonctionnalité prise en charge tant que ses critères d’acceptation sont vérifiés. Ce fichier décrit le produit attendu ; il ne signifie pas que le code est déjà réalisé.
