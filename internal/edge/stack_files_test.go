package edge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Finsys/hawser/internal/config"
	"github.com/Finsys/hawser/internal/docker"
	"github.com/Finsys/hawser/internal/protocol"
)

type fileComposeClient struct {
	*docker.ComposeClient
}

func TestStackFilesEdgeUsesIdenticalServiceAndHTTPStatuses(t *testing.T) {
	client, drain := captureSentMessages(t)
	compose := docker.NewComposeClient("/missing/docker.sock", t.TempDir())
	if err := compose.EnableStackFiles(); err != nil {
		t.Fatal(err)
	}
	client.compose = &fileComposeClient{compose}
	client.cfg = &config.Config{RequestTimeout: 30}
	body, _ := json.Marshal(docker.StackFileRequest{Action: "bind", ProjectName: "demo", ComposeFileNames: []string{"compose.yml"}})
	client.handleRequest(&protocol.RequestMessage{RequestID: "bind-1", Method: "POST", Path: "/_hawser/stack-files", Body: body})
	msgs := drain("response")
	if len(msgs) != 1 || msgs[0].StatusCode != 200 {
		t.Fatalf("Edge binding response: %#v", msgs)
	}
	var result docker.StackFileResponse
	if err := json.Unmarshal([]byte(msgs[0].Body), &result); err != nil || result.Root == "" {
		t.Fatalf("Edge binding body: %#v %v", msgs[0], err)
	}
	code, response := compose.HandleStackFiles(context.Background(), []byte(`{"action":"stat","projectName":"demo","path":"missing.yml"}`))
	if code != 404 || len(response) == 0 {
		t.Fatalf("shared service lost missing status: %d %q", code, response)
	}
}
