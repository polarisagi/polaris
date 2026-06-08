package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAuthManager_Cooldown(t *testing.T) {
	am := NewAuthManager()
	ip := "192.168.1.1"

	// 模拟 3 次失败
	am.RecordFailure(ip)
	am.RecordFailure(ip)
	am.RecordFailure(ip)

	if !am.IsLocked(ip) {
		t.Errorf("Expected IP %s to be locked after 3 failures", ip)
	}

	// 模拟不同 IP 不受影响
	ip2 := "10.0.0.1"
	if am.IsLocked(ip2) {
		t.Errorf("Expected IP %s to not be locked", ip2)
	}

	// 手动修改锁定时间以测试冷却
	am.mu.Lock()
	am.lockedAt[ip] = time.Now().Add(-6 * time.Minute)
	am.mu.Unlock()

	if am.IsLocked(ip) {
		t.Errorf("Expected IP %s to be unlocked after 5 minutes", ip)
	}
}

func TestRateLimitManager_Isolation(t *testing.T) {
	rm := NewRateLimitManager(10, 5) // max burst = 5

	ip1 := "1.1.1.1"
	ip2 := "2.2.2.2"

	// 消耗 ip1 的全部令牌
	for i := 0; i < 5; i++ {
		if !rm.Allow(ip1) {
			t.Errorf("IP1 expected to be allowed at token %d", i)
		}
	}

	// ip1 应该被限流
	if rm.Allow(ip1) {
		t.Errorf("IP1 expected to be rate limited")
	}

	// ip2 不受 ip1 影响
	for i := 0; i < 5; i++ {
		if !rm.Allow(ip2) {
			t.Errorf("IP2 expected to be allowed at token %d", i)
		}
	}
}

func TestAuthMiddleware(t *testing.T) {
	os.Setenv("POLARIS_API_KEY", "super-secret")
	defer os.Unsetenv("POLARIS_API_KEY")

	// 模拟一个 handler，用来检查 Context 是否成功注入
	var extractedAuth *AuthContext
	var mu sync.Mutex

	mockHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		extractedAuth = FromContext(r.Context())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	srv := &Server{}
	handler := srv.withMiddleware(mockHandler)

	tests := []struct {
		name         string
		path         string
		authHeader   string
		expectedCode int
		expectedUser string
	}{
		{
			name:         "Health check skips auth",
			path:         "/healthz",
			authHeader:   "",
			expectedCode: http.StatusOK,
			expectedUser: "anonymous",
		},
		{
			name:         "Missing auth",
			path:         "/v1/agent/query",
			authHeader:   "",
			expectedCode: http.StatusUnauthorized,
			expectedUser: "",
		},
		{
			name:         "Invalid auth",
			path:         "/v1/agent/query",
			authHeader:   "Bearer wrong-key",
			expectedCode: http.StatusUnauthorized,
			expectedUser: "",
		},
		{
			name:         "Valid auth",
			path:         "/v1/agent/query",
			authHeader:   "Bearer super-secret",
			expectedCode: http.StatusOK,
			expectedUser: "admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", tt.path, nil)
			req.RemoteAddr = "127.0.0.1:1234"
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.expectedCode {
				t.Errorf("Expected code %d, got %d", tt.expectedCode, rr.Code)
			}

			if tt.expectedCode == http.StatusOK {
				mu.Lock()
				if extractedAuth == nil || extractedAuth.UserID != tt.expectedUser {
					t.Errorf("Expected UserID %s, got %v", tt.expectedUser, extractedAuth)
				}
				mu.Unlock()
			}
		})
	}
}
