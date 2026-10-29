package metrics

// RecordCredentialDecryptFailed 递增凭证解密失败累计计数器（R2-08）。
func RecordCredentialDecryptFailed() {
	GlobalCredentialDecryptFailedTotal.Add(1)
}
