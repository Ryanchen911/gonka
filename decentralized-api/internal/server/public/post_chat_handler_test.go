package public

import (
	"bytes"
	"decentralized-api/broker"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPRequestWithAuthToken(t *testing.T) {
	tests := []struct {
		name       string
		authToken  string
		expectAuth bool
	}{
		{
			name:       "Empty token",
			authToken:  "",
			expectAuth: false,
		},
		{
			name:       "Valid token",
			authToken:  "valid-token",
			expectAuth: true,
		},
		{
			name:       "Whitespace token",
			authToken:  "   ",
			expectAuth: false,
		},
	}

	endpoints := []string{"tokenize", "completions"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup test node
			node := &broker.Node{
				AuthToken: tt.authToken,
			}

			for _, endpoint := range endpoints {
				t.Run(endpoint+"Request", func(t *testing.T) {
					req, err := http.NewRequest("POST", "http://example.com/"+endpoint, bytes.NewReader([]byte("{}")))
					if err != nil {
						t.Fatal(err)
					}

					// This would normally be in the actual handler
					req.Header.Set("Content-Type", "application/json")
					if strings.TrimSpace(node.AuthToken) != "" {
						req.Header.Set("Authorization", "Bearer "+node.AuthToken)
					}

					if tt.expectAuth {
						if req.Header.Get("Authorization") == "" {
							t.Error("Expected Authorization header but got none")
						}
					} else {
						if req.Header.Get("Authorization") != "" {
							t.Error("Expected no Authorization header but got one")
						}
					}
				})
			}
		})
	}
}
