import { TestBed } from '@angular/core/testing';
import { Component } from '@angular/core';
import { MarkdownComponent } from './markdown';

@Component({
  standalone: true,
  imports: [MarkdownComponent],
  template: `<app-markdown [source]="text" />`,
})
class HostComponent {
  text = '';
}

describe('MarkdownComponent', () => {
  function render(text: string): HTMLElement {
    const fixture = TestBed.createComponent(HostComponent);
    fixture.componentInstance.text = text;
    fixture.detectChanges();
    return fixture.nativeElement as HTMLElement;
  }

  it('renders structure and keeps markup out', () => {
    const root = render(
      '# Title\n\nText with **bold** and a [link](https://example.org).\n\n- a\n- b\n\n```\n<img src=x onerror=alert(1)>\n```\n\n<script>alert(1)</script>',
    );
    expect(root.querySelector('[role="heading"]')?.textContent).toContain('Title');
    expect(root.querySelectorAll('li').length).toBe(2);
    const link = root.querySelector('a') as HTMLAnchorElement;
    expect(link.href).toBe('https://example.org/');
    expect(link.rel).toContain('noopener');
    expect(root.querySelector('pre code')?.textContent).toContain('<img src=x');
    expect(root.querySelector('img')).toBeNull();
    expect(root.querySelector('script')).toBeNull();
    expect(root.textContent).toContain('<script>alert(1)</script>');
  });

  it('renders tables', () => {
    const root = render('| a | b |\n| - | - |\n| 1 | 2 |');
    expect(root.querySelectorAll('th').length).toBe(2);
    expect(root.querySelectorAll('td').length).toBe(2);
  });
});
