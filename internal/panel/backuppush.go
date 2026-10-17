package panel

// Off-site copies of backups: Telegram (owners' chats), WebDAV (PUT into a
// folder) and S3-compatible storage (AWS, Cloudflare R2, MinIO, Backblaze B2…
// signed with SigV4, path-style). Each is used when configured; failures are
// reported, never fatal.

// pushTargets lists the configured destinations.
func (s *Server) pushTargets() []string {
	var out []string
	if s.st.GetSetting("backup_tg", "false") == "true" {
		out = append(out, "telegram")
	}
	if s.st.GetSetting("backup_webdav_url", "") != "" {
		out = append(out, "webdav")
	}
	if s.st.GetSetting("backup_s3_bucket", "") != "" && s.st.GetSetting("backup_s3_access_key", "") != "" {
		out = append(out, "s3")
	}
	return out
}
