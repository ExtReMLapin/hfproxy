import { NgTemplateOutlet } from '@angular/common';
import { Component, computed, input } from '@angular/core';
import { parseMarkdown } from '../../core/markdown';

/**
 * Shows Markdown (a model card) without ever building HTML from it: the text is
 * read into a tree by parseMarkdown and rendered here with the template syntax,
 * which escapes everything.
 */
@Component({
  selector: 'app-markdown',
  standalone: true,
  imports: [NgTemplateOutlet],
  template: `
    <ng-template #inline let-spans>
      @for (span of spans; track $index) {
        @if (span.href) {
          <a
            [href]="span.href"
            target="_blank"
            rel="noopener noreferrer nofollow"
            class="text-emerald-700 underline"
            >{{ span.text }}</a
          >
        } @else if (span.code) {
          <code class="rounded bg-slate-100 px-1 py-0.5 text-[0.85em]">{{ span.text }}</code>
        } @else {
          <span [class.font-semibold]="span.bold" [class.italic]="span.italic">{{
            span.text
          }}</span>
        }
      }
    </ng-template>

    <ng-template #blockList let-blocks>
      @for (block of blocks; track $index) {
        @switch (block.type) {
          @case ('heading') {
            <p
              class="mb-3 mt-6 font-semibold text-slate-900"
              [class.text-xl]="block.level === 1"
              [class.text-lg]="block.level === 2"
              [class.text-base]="block.level >= 3"
              role="heading"
              [attr.aria-level]="block.level"
            >
              <ng-container
                *ngTemplateOutlet="inline; context: { $implicit: block.spans }"
              ></ng-container>
            </p>
          }
          @case ('paragraph') {
            <p class="mb-3 text-sm leading-6 text-slate-700">
              <ng-container
                *ngTemplateOutlet="inline; context: { $implicit: block.spans }"
              ></ng-container>
            </p>
          }
          @case ('code') {
            <pre
              class="mb-3 overflow-x-auto rounded-xl bg-slate-900 p-4 text-xs leading-5 text-slate-100"
            ><code>{{ block.text }}</code></pre>
          }
          @case ('list') {
            @if (block.ordered) {
              <ol class="mb-3 list-decimal space-y-1 pl-6 text-sm text-slate-700">
                @for (item of block.items; track $index) {
                  <li>
                    <ng-container
                      *ngTemplateOutlet="inline; context: { $implicit: item }"
                    ></ng-container>
                  </li>
                }
              </ol>
            } @else {
              <ul class="mb-3 list-disc space-y-1 pl-6 text-sm text-slate-700">
                @for (item of block.items; track $index) {
                  <li>
                    <ng-container
                      *ngTemplateOutlet="inline; context: { $implicit: item }"
                    ></ng-container>
                  </li>
                }
              </ul>
            }
          }
          @case ('quote') {
            <blockquote class="mb-3 border-l-4 border-slate-200 pl-4 text-slate-600">
              <ng-container
                *ngTemplateOutlet="blockList; context: { $implicit: block.blocks }"
              ></ng-container>
            </blockquote>
          }
          @case ('table') {
            <div class="mb-3 overflow-x-auto">
              <table class="w-full text-left text-sm">
                <thead class="bg-slate-50 text-xs text-slate-500">
                  <tr>
                    @for (cell of block.header; track $index) {
                      <th class="p-2">
                        <ng-container
                          *ngTemplateOutlet="inline; context: { $implicit: cell }"
                        ></ng-container>
                      </th>
                    }
                  </tr>
                </thead>
                <tbody>
                  @for (row of block.rows; track $index) {
                    <tr class="border-t border-slate-100">
                      @for (cell of row; track $index) {
                        <td class="p-2">
                          <ng-container
                            *ngTemplateOutlet="inline; context: { $implicit: cell }"
                          ></ng-container>
                        </td>
                      }
                    </tr>
                  }
                </tbody>
              </table>
            </div>
          }
          @case ('rule') {
            <hr class="my-4 border-slate-200" />
          }
        }
      }
    </ng-template>

    <ng-container *ngTemplateOutlet="blockList; context: { $implicit: blocks() }"></ng-container>
  `,
})
export class MarkdownComponent {
  readonly source = input.required<string>();
  protected readonly blocks = computed(() => parseMarkdown(this.source()));
}
