import { Component, input } from '@angular/core';
import { LANGUAGES, LanguageCode, isLanguage, setLanguage, storedLanguage } from '../../core/i18n';
import { IconComponent } from '../icon/icon';

@Component({
  selector: 'app-language-switcher',
  standalone: true,
  imports: [IconComponent],
  template: `
    <label
      class="relative inline-flex items-center"
      [class]="tone() === 'dark' ? 'text-white/60' : 'text-slate-500'"
    >
      <app-icon name="globe" [size]="15" class="pointer-events-none absolute left-2.5" />
      <select
        class="cursor-pointer appearance-none rounded-lg border bg-transparent py-1.5 pl-8 pr-3 text-xs font-semibold outline-none focus-visible:ring-2 focus-visible:ring-emerald-600"
        [class]="
          tone() === 'dark'
            ? 'border-white/15 hover:bg-white/10 hover:text-white'
            : 'border-slate-200 hover:bg-slate-50 hover:text-slate-900'
        "
        i18n-aria-label
        aria-label="Language"
        (change)="choose($event)"
      >
        @for (language of languages; track language.code) {
          <option
            class="text-slate-900"
            [value]="language.code"
            [selected]="language.code === current"
          >
            {{ language.name }}
          </option>
        }
      </select>
    </label>
  `,
})
export class LanguageSwitcherComponent {
  readonly tone = input<'light' | 'dark'>('light');
  protected readonly languages = LANGUAGES;
  protected readonly current: LanguageCode = storedLanguage();

  protected choose(event: Event): void {
    const value = (event.target as HTMLSelectElement).value;
    if (isLanguage(value) && value !== this.current) setLanguage(value);
  }
}
