package appws

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestDialManagerIntegration(t *testing.T) {
	if os.Getenv("APPWS_INTEGRATION") == "" {
		t.Skip("APPWS_INTEGRATION not set")
	}

	url := os.Getenv("APPWS_MANAGER_URL")
	if url == "" {
		url = "wss://afi-test1.innovaphone.cloud/manager"
	}

	password := os.Getenv("APPWS_MANAGER_PASSWORD")
	if password == "" {
		t.Fatal("APPWS_MANAGER_PASSWORD not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := Dial(ctx, Config{
		URL:      url,
		App:      "manager",
		Password: password,
		SIP:      "manager",
		GUID:     "00000000000000000000000000000000",
		DN:       "Admin",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close(context.Background())

	var result struct {
		Version string `json:"version"`
	}

	src := fmt.Sprintf("it-%d", time.Now().UnixNano())
	if err := client.Do(ctx, "", "AppPlatformInfoRequest", map[string]any{}, &result, WithSrc(src)); err != nil {
		t.Fatalf("AppPlatformInfoRequest: %v", err)
	}

	if result.Version == "" {
		t.Fatalf("empty version in AppPlatformInfoResult")
	}
}
