import { LOCALE_ID } from '@angular/core';
import { bootstrapApplication } from '@angular/platform-browser';
import { appConfig } from './app/app.config';
import { App } from './app/app';
import { loadLanguage } from './app/core/i18n';

loadLanguage()
  .then((language) => {
    document.title = $localize`hfproxy · Admin console`;
    return bootstrapApplication(App, {
      ...appConfig,
      providers: [...appConfig.providers, { provide: LOCALE_ID, useValue: language }],
    });
  })
  .catch((err) => console.error(err));
