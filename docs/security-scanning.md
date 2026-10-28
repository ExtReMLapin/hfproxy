# Analyses de sécurité en CI

La CI exécute `gosec`, `staticcheck`, `govulncheck`, `npm audit --omit=dev` et un scan Trivy de l'image Docker (HIGH et CRITICAL corrigeables) sur chaque pull request.

## Règles gosec exclues

Les règles suivantes sont désactivées parce que les alertes ont été relues une à une et ne représentent pas de risque dans ce code. Toute autre règle reste active et bloque le build.

| Règle | Pourquoi elle est exclue |
| --- | --- |
| G115 | Conversions entières vers `byte`/`uint` bornées en amont : longueurs SOCKS validées (≤ 255) par la configuration, ports ≤ 65535, tailles positives. |
| G118 | Le préchargement d'un dépôt continue volontairement après la requête HTTP (tâche de fond avec son propre contexte). |
| G124 | Les cookies sont tous posés avec `Secure` selon `PUBLIC_URL`, `SameSite=Lax` et `HttpOnly` ; seul `hfproxy_csrf` n'est pas `HttpOnly`, car l'interface doit le relire (double submit). |
| G202, G701 | Concaténation de SQL limitée aux fragments `WHERE`/`LIMIT` construits par le code ; toutes les valeurs passent par des paramètres `$n`. |
| G304, G703 | Les chemins lus ou supprimés viennent de la configuration de l'opérateur (`*_FILE`) ou de fichiers temporaires créés par le service. |
| G404 | Le générateur non cryptographique ne sert qu'à répartir (jitter) les nouvelles tentatives vers l'amont. |
| G704 | Les requêtes sortantes vers l'amont sont voulues : hôtes autorisés par liste, dialer qui refuse les adresses internes, redirections contrôlées. |
| G705 | Les contenus écrits sont des fichiers statiques de l'interface, pas des données utilisateur interprétées. |
