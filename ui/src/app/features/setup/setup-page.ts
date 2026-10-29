import { Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { IconComponent } from '../../shared/icon/icon';
import { LanguageSwitcherComponent } from '../../shared/language-switcher/language-switcher';

/** Shortest password the service accepts; it checks the same rule. */
export const MIN_PASSWORD_LENGTH = 12;

@Component({
  selector: 'app-setup-page',
  standalone: true,
  imports: [FormsModule, IconComponent, LanguageSwitcherComponent],
  templateUrl: './setup-page.html',
})
export class SetupPageComponent {
  protected readonly auth = inject(AuthService);
  protected readonly minPasswordLength = MIN_PASSWORD_LENGTH;
  protected form = {
    setupCode: '',
    username: '',
    displayName: '',
    email: '',
    password: '',
    confirmation: '',
  };
  protected readonly showPassword = signal(false);
  /** What stopped the last submission before anything was sent. */
  protected readonly problem = signal('');

  protected validate(): string {
    const form = this.form;
    if (!form.setupCode.trim()) return $localize`Enter the setup code.`;
    if (!/^[A-Za-z0-9._-]{1,64}$/.test(form.username.trim())) {
      return $localize`Use 1 to 64 letters, digits, dots, underscores, or hyphens for the username.`;
    }
    if (form.password.length < MIN_PASSWORD_LENGTH) {
      return $localize`The password must contain at least ${MIN_PASSWORD_LENGTH}:MIN: characters.`;
    }
    if (form.password !== form.confirmation) return $localize`The two passwords do not match.`;
    return '';
  }

  protected async submit(): Promise<void> {
    this.auth.error.set('');
    this.problem.set(this.validate());
    if (this.problem()) return;
    await this.auth.createFirstAdmin({
      setup_code: this.form.setupCode.trim(),
      username: this.form.username.trim(),
      display_name: this.form.displayName.trim(),
      email: this.form.email.trim(),
      password: this.form.password,
    });
    if (this.auth.error()) this.form.password = this.form.confirmation = '';
  }
}
