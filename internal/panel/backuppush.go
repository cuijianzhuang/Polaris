package panel

// Off-site copies of backups: Telegram (owners' chats), WebDAV (PUT into a
// folder), S3-compatible storage (AWS, Cloudflare R2, MinIO, Backblaze B2…
// signed with SigV4, path-style) and GitHub Gist (encrypted archives only).
// Each is used when configured; failures are reported, never fatal.

// pushTargets lists the configured destinations.
func (s *Server) pushTargets() []string {
	var out []string
	if s.st.GetSetting("backup_tg", "false") == "true" {
		out = append(out, "telegram")
	}
	if s.st.GetSetting("backup_webdav_url", "") != "" {
		out = append(out, "webdav")
	}
	// Skipped when the bucket is already the backup store.
	if s.st.GetSetting("backup_store", "local") != "s3" && s.st.GetSetting("backup_s3_bucket", "") != "" && s.st.GetSetting("backup_s3_access_key", "") != "" {
		out = append(out, "s3")
	}
	// Gist only ever receives encrypted archives: without a backup
	// passphrase the target stays inactive (the settings page says why).
	if s.gistConfigured() && s.backupPassphrase() != "" {
		out = append(out, "gist")
	}
	return out
}

// gistConfigured reports whether the Gist target is switched on with a token.
func (s *Server) gistConfigured() bool {
	return s.st.GetSetting("backup_gist", "false") == "true" && s.st.GetSetting("backup_gist_token", "") != ""
}
