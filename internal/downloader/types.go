package downloader

// URLValidator is a function type for validating URLs before downloading.
type URLValidator func(rawURL string) error
