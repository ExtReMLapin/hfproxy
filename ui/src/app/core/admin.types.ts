export type Page =
  'overview' | 'repositories' | 'cache' | 'users' | 'groups' | 'audit' | 'settings';
export type RepoType = 'models' | 'datasets' | 'spaces';
export type Policy = 'public' | 'allowlist' | 'admin_only';

export interface Principal {
  id: string;
  username: string;
  email?: string;
  display_name?: string;
  role: string;
  authenticated?: boolean;
}

export interface AuthProviders {
  local: boolean;
  oidc: boolean;
  ldap: boolean;
}
export interface Stats {
  repositories: number;
  artifacts: number;
  cache_bytes: number;
  cache_size: string;
  cache_hits: number;
  cache_misses: number;
  hit_ratio: number;
  downloads_in_progress: number;
}

export interface ActiveDownload {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  path: string;
  bytes: number;
  started_at: string;
}

export interface PreloadJob {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  status:
    | 'queued'
    | 'running'
    | 'completed'
    | 'completed_with_errors'
    | 'failed'
    | 'interrupted'
    | 'incomplete';
  files_total: number;
  files_processed: number;
  failed_files: number;
  error?: string;
  started_at: string;
  updated_at: string;
}

export interface Repository {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  access_policy: Policy;
  created_at: string;
  updated_at: string;
}

export interface CacheEntry {
  id: string;
  repo_type: RepoType;
  repo_id: string;
  commit_sha: string;
  path: string;
  size_bytes: number;
  sha256: string;
  cached_at: string;
  last_accessed_at?: string;
}

export interface AdminUser {
  id: string;
  username: string;
  email: string;
  display_name: string;
  role: string;
  status: string;
  created_at: string;
}

export interface AdminGroup {
  id: string;
  name: string;
  source: string;
  members: number;
}
export interface GroupMember {
  id: string;
  username: string;
  role: string;
}
export interface Grant {
  subject_type: string;
  subject_id: string;
  permission: string;
}
export interface AuditItem {
  id: string;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  created_at: string;
}

export interface ManagedSettings {
  public_url: string;
  hf_base_url: string;
  hf_proxy_set: boolean;
  hf_allowed_hosts: string[];
  hf_timeout: string;
  hf_token_set: boolean;
  s3_endpoint: string;
  s3_region: string;
  s3_bucket: string;
  s3_prefix: string;
  s3_force_path_style: boolean;
  s3_server_side_encryption: boolean;
  s3_access_key_set: boolean;
  s3_secret_key_set: boolean;
  auth_mode: 'local' | 'oidc';
  anonymous_public_read: boolean;
  session_idle_ttl: string;
  session_absolute_ttl: string;
  cache_max_file_size: number;
  cache_ref_ttl: string;
  cache_negative_ttl: string;
  cache_gc_grace: string;
  oidc_issuer: string;
  oidc_client_id: string;
  oidc_client_secret_set: boolean;
  oidc_redirect_url: string;
  oidc_auto_provision: boolean;
  ldap_url: string;
  ldap_starttls: boolean;
  ldap_bind_dn: string;
  ldap_bind_password_set: boolean;
  ldap_base_dn: string;
  ldap_user_filter: string;
  ldap_auto_provision: boolean;
  metrics_enabled: boolean;
  metrics_path: string;
  tracing_enabled: boolean;
  tracing_service_name: string;
  tracing_sampling_ratio: number;
  otlp_endpoint: string;
  cache_inventory_refresh: string;
}

export interface SettingsDraft extends ManagedSettings {
  hf_allowed_hosts_text: string;
  hf_proxy_url: string;
  clear_hf_proxy: boolean;
  hf_token: string;
  clear_hf_token: boolean;
  s3_access_key: string;
  s3_secret_key: string;
  clear_s3_credentials: boolean;
  oidc_client_secret: string;
  clear_oidc_client_secret: boolean;
  ldap_bind_password: string;
  clear_ldap_bind_password: boolean;
}
