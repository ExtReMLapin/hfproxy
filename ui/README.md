# hfproxy admin UI

Angular 22 administration console. Tailwind CSS 4 is processed by Angular through `.postcssrc.json`.

## Local development

Use the Node release pinned in `.nvmrc`, install the locked dependencies, then start the Angular dev server:

```sh
nvm use
npm ci
npm start
```

The dev server runs at `http://localhost:4200` and proxies API requests to the Go service on `http://127.0.0.1:8080`.

## Production build

```sh
npm run build
```

The production assets are written to `dist/hfproxy/browser` and embedded in the Go server image.
