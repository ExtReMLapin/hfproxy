import { Component, OnInit, inject } from '@angular/core';
import { RouterOutlet } from '@angular/router';
import { AuthService } from './core/auth.service';
import { LoginPageComponent } from './features/login/login-page';

@Component({
  selector: 'app-root',
  standalone: true,
  imports: [RouterOutlet, LoginPageComponent],
  templateUrl: './app.html',
})
export class App implements OnInit {
  protected readonly auth = inject(AuthService);

  ngOnInit(): void {
    void this.auth.initialize();
  }
}
