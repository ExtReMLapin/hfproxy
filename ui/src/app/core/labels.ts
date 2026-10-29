/** Display names of values the API returns as identifiers. Unknown values are shown as received. */

export function roleLabel(role: string | undefined): string {
  switch (role) {
    case 'reader':
      return $localize`Reader`;
    case 'operator':
      return $localize`Operator`;
    case 'admin':
      return $localize`Admin`;
    default:
      return role ?? '';
  }
}

export function permissionLabel(permission: string): string {
  switch (permission) {
    case 'read':
      return $localize`Read`;
    default:
      return permission;
  }
}

export function subjectLabel(subjectType: string): string {
  switch (subjectType) {
    case 'user':
      return $localize`User`;
    case 'group':
      return $localize`Group`;
    default:
      return subjectType;
  }
}

export function sourceLabel(source: string): string {
  return source === 'local' ? $localize`Local` : source.toUpperCase();
}
