# EmojiVer

SemVer stays the canonical version: OCI tags, Go build flags and the
User-Agent header only accept ASCII. EmojiVer is a reversible rendering of it,
computed by the release workflow and embedded in the binary.

```
🐙 1️⃣🔹0️⃣ 🔒 🌖 🟢 👍🏻 🍐🍑🍋🍋        (0.1.0, spaces added for reading)
│  │       │  │  │  │   └ checksum: first 16 bits of SHA-256("0.1.0\n" + commit), as hex fruits
│  │       │  │  │  └ channel: 🏻 stable 🏼 rc 🏽 beta 🏾 alpha 🏿 nightly (prereleases add 🧪N)
│  │       │  │  └ health: open "Still broken after fix" issues, 🟢 0, 🟡 1-5, 🔴 more
│  │       │  └ moon phase of the release commit date (UTC)
│  │       └ nature of the commits since the previous tag: 💥 breaking > 🔒 security > ✨ feature > 🐛 fix > 🧹 chore
│  └ minor 🔹 patch, in keycaps
└ major codename, from internal/emojiver/emojidex.json (append-only)
```

Unknown values (local builds) render as ❔ nature, 🌚 moon and ⚪ health.

| Form | Example | Where |
| --- | --- | --- |
| Emoji | `🐙1️⃣🔹0️⃣🔒🌖🟢👍🏻🍐🍑🍋🍋` | `hfproxy version --emoji`, `GET /api/v1/version`, image label `io.hfproxy.emojiver` |
| Punycode (RFC 3492) | `hfproxy version --tag` | extra image tag `ghcr.io/hfproxy/hfproxy:xn--…` |
| RFC 8187 | `UTF-8''%F0%9F%90%99…` | `X-HFProxy-Version-Emoji` response header |
| CLDR names | `octopus, one, dot, zero, locked, …` | `hfproxy version --a11y` |

`go run ./cmd/emojiver decode <emoji or xn--tag>` prints every field;
`go run ./cmd/emojiver verify --commit <sha> <emoji>` checks the checksum.

Rules: keycaps need U+FE0F, no leading zeros, every glyph is a single code
point from Emoji 13.0 or older (plus the skin-tone modifier), and a codename
in `emojidex.json` is never changed or reused.
