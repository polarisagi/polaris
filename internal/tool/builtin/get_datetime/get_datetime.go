package get_datetime

import (
	"context"
	"encoding/json"
	"time"
)

func GetDatetimeFn(_ context.Context, _ []byte) ([]byte, error) {
	now := time.Now()
	result := map[string]any{
		"utc":      now.UTC().Format(time.RFC3339),
		"local":    now.Format(time.RFC3339),
		"unix":     now.Unix(),
		"timezone": now.Location().String(),
	}
	return json.Marshal(result) //nolint:wrapcheck
}
