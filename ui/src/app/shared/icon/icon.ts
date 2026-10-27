import { Component, Input } from '@angular/core';

@Component({
  selector: 'app-icon',
  standalone: true,
  template: `
    <svg
      [attr.width]="size"
      [attr.height]="size"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.8"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
    >
      @switch (name) {
        @case ('grid') {
          <rect x="3.5" y="3.5" width="7" height="7" rx="1.5" />
          <rect x="13.5" y="3.5" width="7" height="7" rx="1.5" />
          <rect x="3.5" y="13.5" width="7" height="7" rx="1.5" />
          <rect x="13.5" y="13.5" width="7" height="7" rx="1.5" />
        }
        @case ('box') {
          <path d="m12 3 8.5 4.5v9L12 21l-8.5-4.5v-9L12 3Z" />
          <path d="m3.8 7.7 8.2 4.5 8.2-4.5M12 12.2V21" />
        }
        @case ('database') {
          <ellipse cx="12" cy="5" rx="8.5" ry="3" />
          <path
            d="M3.5 5v7c0 1.7 3.8 3 8.5 3s8.5-1.3 8.5-3V5M3.5 12v7c0 1.7 3.8 3 8.5 3s8.5-1.3 8.5-3v-7"
          />
        }
        @case ('users') {
          <path d="M16 20v-1.5a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4V20" />
          <circle cx="9.5" cy="7" r="3.5" />
          <path d="M17 11a3.5 3.5 0 1 0-1-6.9M21 20v-1.5a4 4 0 0 0-3-3.9" />
        }
        @case ('layers') {
          <path d="m12 3 9 5-9 5-9-5 9-5Z" />
          <path d="m3 12 9 5 9-5M3 16l9 5 9-5" />
        }
        @case ('shield') {
          <path d="M12 3 20 6v5.5c0 5-3.3 8-8 9.5-4.7-1.5-8-4.5-8-9.5V6l8-3Z" />
          <path d="m9 12 2 2 4-4" />
        }
        @case ('settings') {
          <circle cx="12" cy="12" r="3" />
          <path
            d="m19.4 15 .1.1 1.4 1.1-1.4 2.4-1.7-.6a8 8 0 0 1-1.8 1l-.3 1.8h-2.8l-.3-1.8a8 8 0 0 1-1.8-1l-1.7.6-1.4-2.4L7.1 15a8 8 0 0 1 0-2l-1.4-1.1 1.4-2.4 1.7.6a8 8 0 0 1 1.8-1l.3-1.8h2.8l.3 1.8a8 8 0 0 1 1.8 1l1.7-.6 1.4 2.4L19.4 13a8 8 0 0 1 0 2Z"
            transform="translate(-1 -1) scale(1.08)"
          />
        }
        @case ('search') {
          <circle cx="10.8" cy="10.8" r="6.8" />
          <path d="m16 16 4.5 4.5" />
        }
        @case ('plus') {
          <path d="M12 5v14M5 12h14" />
        }
        @case ('chevron') {
          <path d="m9 18 6-6-6-6" />
        }
        @case ('arrow') {
          <path d="M7 17 17 7M8 7h9v9" />
        }
        @case ('logout') {
          <path d="M10 17l5-5-5-5M15 12H3" />
          <path d="M12 3h6a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-6" />
        }
        @case ('menu') {
          <path d="M4 6h16M4 12h16M4 18h16" />
        }
        @case ('check') {
          <path d="m5 12 4 4L19 6" />
        }
        @case ('refresh') {
          <path d="M20 7v5h-5M4 17v-5h5" />
          <path d="M5.6 9A7 7 0 0 1 18 6.5L20 12M4 12l2 5.5A7 7 0 0 0 18.4 15" />
        }
        @case ('trash') {
          <path d="M4 7h16M10 11v6M14 11v6M5.5 7l1 13h11l1-13M9 7V4h6v3" />
        }
        @case ('key') {
          <circle cx="8" cy="15" r="4" />
          <path d="m11 12 9-9M16 7l2 2M14 9l2 2" />
        }
        @case ('clock') {
          <circle cx="12" cy="12" r="9" />
          <path d="M12 7v5l3 2" />
        }
        @case ('check-circle') {
          <circle cx="12" cy="12" r="9" />
          <path d="m8 12 2.5 2.5L16 9" />
        }
        @case ('alert') {
          <path
            d="M10.3 4.3 2.8 17.2A2 2 0 0 0 4.5 20h15a2 2 0 0 0 1.7-2.8L13.7 4.3a2 2 0 0 0-3.4 0Z"
          />
          <path d="M12 9v4M12 17h.01" />
        }
        @case ('eye') {
          <path d="M2.5 12s3.3-6 9.5-6 9.5 6 9.5 6-3.3 6-9.5 6-9.5-6-9.5-6Z" />
          <circle cx="12" cy="12" r="2.5" />
        }
        @case ('lock') {
          <rect x="4" y="10" width="16" height="11" rx="2" />
          <path d="M8 10V7a4 4 0 0 1 8 0v3M12 14v3" />
        }
      }
    </svg>
  `,
})
export class IconComponent {
  @Input({ required: true }) name = '';
  @Input() size = 20;
}
