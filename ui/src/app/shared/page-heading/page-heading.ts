import { Component, Input } from '@angular/core';

@Component({
  selector: 'app-page-heading',
  standalone: true,
  template: `
    <div class="mb-7 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
      <div>
        <p class="mb-2 text-xs font-bold uppercase tracking-[0.18em] text-emerald-700">
          {{ eyebrow }}
        </p>
        <h1 class="text-2xl font-semibold tracking-tight text-slate-950 sm:text-[2rem]">
          {{ title }}
        </h1>
        @if (subtitle) {
          <p class="mt-2 max-w-2xl text-sm leading-6 text-slate-500">{{ subtitle }}</p>
        }
      </div>
      <div class="flex shrink-0 items-center gap-2"><ng-content /></div>
    </div>
  `,
})
export class PageHeadingComponent {
  @Input({ required: true }) title = '';
  @Input() subtitle = '';
  @Input() eyebrow = 'Workspace';
}
