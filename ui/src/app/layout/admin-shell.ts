import { Component, inject, signal } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { NavigationEnd, Router, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { filter } from 'rxjs';
import { AuthService } from '../core/auth.service';
import { FeedbackService } from '../core/feedback.service';
import { roleLabel } from '../core/labels';
import { IconComponent } from '../shared/icon/icon';
import { LanguageSwitcherComponent } from '../shared/language-switcher/language-switcher';

interface NavItem {
  path: string;
  label: string;
  icon: string;
  personal?: boolean;
}

@Component({
  selector: 'app-admin-shell',
  standalone: true,
  imports: [RouterOutlet, RouterLink, RouterLinkActive, IconComponent, LanguageSwitcherComponent],
  templateUrl: './admin-shell.html',
})
export class AdminShellComponent {
  protected readonly auth = inject(AuthService);
  protected readonly feedback = inject(FeedbackService);
  private readonly router = inject(Router);
  protected readonly path = signal('/overview');

  protected readonly navItems: NavItem[] = [
    { path: '/overview', label: $localize`Overview`, icon: 'grid', personal: true },
    { path: '/tokens', label: $localize`Access tokens`, icon: 'shield', personal: true },
    { path: '/repositories', label: $localize`Repositories`, icon: 'box' },
    { path: '/cache', label: $localize`Cache`, icon: 'database' },
    { path: '/users', label: $localize`Users`, icon: 'users' },
    { path: '/groups', label: $localize`Groups`, icon: 'layers' },
    { path: '/audit', label: $localize`Audit log`, icon: 'shield' },
    { path: '/settings', label: $localize`Settings`, icon: 'settings' },
  ];

  constructor() {
    this.router.events
      .pipe(filter((event) => event instanceof NavigationEnd))
      .pipe(takeUntilDestroyed())
      .subscribe(() => this.path.set(this.router.url));
    this.path.set(this.router.url);
  }

  protected get visibleNavItems(): NavItem[] {
    return this.auth.principal()?.role === 'admin'
      ? this.navItems
      : this.navItems.filter((item) => item.personal);
  }

  protected get currentTitle(): string {
    const route = this.path();
    return this.navItems.find((item) => route.startsWith(item.path))?.label ?? $localize`Overview`;
  }

  protected get initials(): string {
    const name = this.auth.principal()?.display_name || this.auth.principal()?.username || 'H';
    return name.slice(0, 1).toUpperCase();
  }

  protected readonly roleLabel = roleLabel;

  protected async signOut(): Promise<void> {
    await this.auth.signOut();
    // The router outlet is recreated at the next sign-in and would show the page
    // the previous user was on, whatever role the next one has; guards only run
    // during a navigation.
    if (!this.auth.principal()) await this.router.navigateByUrl('/overview');
  }
}
