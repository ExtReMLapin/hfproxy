import { Injectable, signal } from '@angular/core';

@Injectable({ providedIn: 'root' })
export class FeedbackService {
  readonly success = signal('');
  readonly error = signal('');

  showSuccess(message: string): void {
    this.error.set('');
    this.success.set(message);
    window.setTimeout(() => {
      if (this.success() === message) this.success.set('');
    }, 4500);
  }

  showError(message: string): void {
    this.success.set('');
    this.error.set(message);
  }

  clear(): void {
    this.success.set('');
    this.error.set('');
  }
}
