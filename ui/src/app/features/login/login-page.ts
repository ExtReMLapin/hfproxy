import { Component, inject } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { AuthService } from '../../core/auth.service';
import { IconComponent } from '../../shared/icon/icon';

@Component({
  selector: 'app-login-page',
  standalone: true,
  imports: [FormsModule, IconComponent],
  templateUrl: './login-page.html',
})
export class LoginPageComponent {
  protected readonly auth = inject(AuthService);
  protected credentials = { username: '', password: '' };

  protected submit(): void {
    void this.auth.signIn(this.credentials.username.trim(), this.credentials.password);
  }
}
