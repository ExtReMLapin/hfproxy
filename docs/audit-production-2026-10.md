# Audit de préparation à la production — 2026-10-29

Audit final réalisé après la résolution des issues #159–#191. Évaluation technique, sans complaisance.

## 1. Vérifications réellement exécutées

| Vérification | Résultat |
|---|---|
| `go build`, `go vet`, `gofmt` | propres |
| `go test -race -count=1 ./...` (Postgres 16 réel + S3 moto) | tous les paquets passent |
| `staticcheck 2026.2.2` | aucune remarque |
| `gosec v2.30.0` (exclusions documentées dans `docs/security-scanning.md`) | aucune alerte |
| Couverture Go (instructions) | 65,3 % (seuil CI : 60 %) |
| Tests UI Angular (18), ESLint, `npm audit` prod | verts |
| CI sur `main` (build, staticcheck, gosec, vulncheck, docker + smoke + Trivy, chart, ui) | verte au dernier commit audité (520e501), run du dernier commit : voir l'historique |
| Workflow nocturne charge + chaos (déclenché à la main) | succès (run 40793884956) |
| E2E binaire réel + Postgres + S3 + faux Hub + `huggingface_hub` 0.30.2 | fichiers petits et LFS téléchargés et mis en cache |
| E2E navigateur (Chromium) : login + 8 pages | sans erreur CSP ni console |
| LDAPS (OpenLDAP réel) : provisioning, déplacement de DN, sync de groupes | OK |
| OIDC | fournisseur simulé uniquement (tests automatisés) |

Non exécuté : `govulncheck` en local (vuln.go.dev inaccessible depuis le sandbox ; il tourne en CI), installation du chart Helm sur un vrai cluster.

## 2. Constats de l'audit

### Critique
Aucun constat critique ouvert.

### Élevée
- **Chart Helm jamais installé sur un cluster.** Il est lint/rendu en CI et sa configuration est validée par le binaire, mais aucun `helm install` réel n'a eu lieu. Un défaut de déploiement (RBAC, volumes, hook de migration) ne serait découvert qu'en production. *Action : installer sur un cluster de préproduction (kind/k3d suffit) avant tout déploiement.*
- **Aucun test avec de vrais fichiers de plusieurs Go.** La charge nocturne utilise des dizaines de MiB. Le comportement sur des fichiers de 10–100 Go (durée des fills, quota de staging, reprise) n'est pas mesuré. *Action : un essai manuel avec un gros modèle réel et le vrai Hub.*

### Moyenne
- **Hub réel jamais contacté** : tout a été testé contre un faux Hub (port 80, car les ports amont sont restreints à 443/loopback:80/hôte configuré). Les cas limites du vrai Hub (redirections CDN, en-têtes `X-Linked-*`, Xet) ne sont validés que par les tests et le client `huggingface_hub`.
- **Fenêtres de cache d'authentification et de file de journaux** : le cache de credentials (5 s) est purgé à la révocation sur l'instance qui la traite, mais une autre réplique peut accepter un jeton révoqué jusqu'à 5 s. La file asynchrone des téléchargements peut perdre des lignes de journal en cas de crash (vidée proprement à l'arrêt).
- **Pas de limitation de débit sur les téléchargements** (seuls les logins sont limités) : un utilisateur authentifié peut saturer la bande passante amont.
- **Dépendance unique à Postgres et S3** pour les lectures en cache : si l'un est indisponible, même les hits échouent (les tests de chaos vérifient l'absence de corruption, pas la continuité de service).
- **OIDC/LDAP configurés par un administrateur** déclenchent des connexions sortantes sans garde SSRF (l'administrateur est de confiance, documenté dans `docs/threat-model.md`).
- **Premier administrateur uniquement par CLI** (`hfproxy user create`), sans parcours de bootstrap dans l'UI.

### Faible
- Requêtes de statistiques parcourant la table `downloads` (rétention configurable, index présents ; à surveiller à fort volume).
- Mélange français/anglais dans l'interface.
- Noms d'utilisateur auto-provisionnés LDAP/OIDC avec suffixe aléatoire peu lisibles.
- Chemin « compte LDAP désactivé » testé uniquement en test unitaire, pas contre un annuaire réel.
- Dependabot : ne pas fusionner TypeScript 7 (PR #193/#194/#200) tant que Angular 22 ne le supporte pas ; `typescript-eslint` 8.72 à regrouper avec la prochaine mise à jour compatible.
- Reproductibilité de l'image Docker non vérifiée (seul le binaire est comparé deux fois en CI).

## 3. Corrections faites pendant l'audit (bugs réels trouvés par les E2E)
- Fills en streaming interrompus quand le client raccroche juste après le dernier octet : fichiers petits non mis en cache, téléchargement bloqué « en cours ». Corrigé + test de régression + nettoyage des téléchargements abandonnés (12 h).
- `/metrics` anonyme sur le port public : désormais réservé aux opérateurs.
- Lignes DEBUG du SDK AWS par hit : silencées.
- `HF_BASE_URL` avec port non standard inutilisable : corrigé.
- Nom d'utilisateur non borné dans l'audit des échecs de login : plafonné.
- Couche d'accès aux données : le SQL des handlers est déplacé vers `internal/db` (reste volontairement dans `server.go` : sauvegarde des réglages, ping/état des migrations).

## 4. Risques résiduels à surveiller
Taux d'erreur amont et état du disjoncteur (alertes fournies), espace de staging (`hfproxy_staging_free_bytes`), file du journal de téléchargements pleine (repli synchrone), volume de la table `downloads`, expiration des certificats LDAPS/OIDC.

## 5. Verdict

**NON PRÊT pour la production, au sens strict** : le code et les tests automatisés sont solides, mais deux points de validation que seul un environnement réel peut apporter manquent (installation du chart sur un cluster ; essai avec le vrai Hub et un très gros modèle). Ce sont des vérifications à faire, pas des défauts connus du code. Une fois ces deux essais réussis et les risques « moyenne » acceptés, le projet peut être déployé en production avec un premier déploiement surveillé.
