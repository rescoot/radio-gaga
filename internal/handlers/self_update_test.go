package handlers

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"radio-gaga/internal/models"
)

// MockCommandHandlerClient for testing
type MockCommandHandlerClient struct{}

func (m *MockCommandHandlerClient) SendCommandResponse(requestID, status, message string) {
}

func (m *MockCommandHandlerClient) CleanRetainedMessage(topic string) error {
	return nil
}

func (m *MockCommandHandlerClient) GetCommandParam(command, param string, defaultValue interface{}) interface{} {
	return defaultValue
}

func (m *MockCommandHandlerClient) PublishTelemetryData(current *models.TelemetryData) error {
	return nil
}

func (m *MockCommandHandlerClient) GetConfigPath() string {
	return "/tmp/test-config.yml"
}

func TestHandleSelfUpdateCommand(t *testing.T) {
	// Create a temporary directory for test files
	tempDir, err := os.MkdirTemp("", "radio-gaga-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a mock binary file
	mockBinary := []byte("mock binary content for testing")
	mockBinaryHash := sha256.Sum256(mockBinary)
	mockBinaryChecksum := fmt.Sprintf("%x", mockBinaryHash[:])

	// Create HTTP server to serve the mock binary
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(mockBinary)
	}))
	defer server.Close()

	tests := []struct {
		name        string
		params      map[string]interface{}
		config      *models.Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "successful update",
			params: map[string]interface{}{
				"url":      server.URL,
				"checksum": fmt.Sprintf("sha256:%s", mockBinaryChecksum),
			},
			config: &models.Config{
				ServiceName: "test-service",
			},
			expectError: false,
		},
		{
			name: "missing URL",
			params: map[string]interface{}{
				"checksum": fmt.Sprintf("sha256:%s", mockBinaryChecksum),
			},
			config: &models.Config{
				ServiceName: "test-service",
			},
			expectError: true,
			errorMsg:    "update URL not specified or invalid",
		},
		{
			name: "missing checksum",
			params: map[string]interface{}{
				"url": server.URL,
			},
			config: &models.Config{
				ServiceName: "test-service",
			},
			expectError: true,
			errorMsg:    "checksum not specified or invalid",
		},
		{
			name: "invalid checksum format",
			params: map[string]interface{}{
				"url":      server.URL,
				"checksum": "invalid-checksum-format",
			},
			config: &models.Config{
				ServiceName: "test-service",
			},
			expectError: true,
			errorMsg:    "invalid checksum format",
		},
		{
			name: "checksum mismatch",
			params: map[string]interface{}{
				"url":      server.URL,
				"checksum": "sha256:wrongchecksum",
			},
			config: &models.Config{
				ServiceName: "test-service",
			},
			expectError: true,
			errorMsg:    "checksum mismatch",
		},
	}

	client := &MockCommandHandlerClient{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := handleSelfUpdateCommand(client, tt.params, "test-request-id", tt.config)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error containing '%s', got: %v", tt.errorMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}
		})
	}
}

func TestCreateUpdateHelperScript(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "radio-gaga-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	newBinaryPath := filepath.Join(tempDir, "new-binary")
	currentBinaryPath := filepath.Join(tempDir, "current-binary")
	serviceName := "test-service"

	// Create mock binary files
	if err := os.WriteFile(newBinaryPath, []byte("new binary"), 0755); err != nil {
		t.Fatalf("Failed to create new binary file: %v", err)
	}
	if err := os.WriteFile(currentBinaryPath, []byte("current binary"), 0755); err != nil {
		t.Fatalf("Failed to create current binary file: %v", err)
	}

	scriptPath, err := createUpdateHelperScript(newBinaryPath, currentBinaryPath, serviceName)
	if err != nil {
		t.Fatalf("Failed to create update helper script: %v", err)
	}
	defer os.Remove(scriptPath)

	// Verify script file was created
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("Script file was not created: %v", err)
	}

	// Read and verify script content
	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("Failed to read script content: %v", err)
	}

	scriptStr := string(scriptContent)

	// Verify script contains expected elements
	expectedElements := []string{
		"#!/bin/sh",
		"Radio-Gaga update helper script",
		newBinaryPath,
		currentBinaryPath,
		serviceName,
		"error_cleanup",
		"systemctl restart",
		"systemctl is-active",
		"NEEDS_REMOUNT",
		"mount -o remount",
	}

	for _, element := range expectedElements {
		if !strings.Contains(scriptStr, element) {
			t.Errorf("Script missing expected element: %s", element)
		}
	}

	// Verify script is executable
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("Failed to stat script file: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("Script is not executable")
	}
}

func TestUpdateHelperScriptErrorHandling(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "radio-gaga-error-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Test with non-existent paths
	nonExistentPath := filepath.Join(tempDir, "non-existent")
	serviceName := "test-service"

	scriptPath, err := createUpdateHelperScript(nonExistentPath, nonExistentPath, serviceName)
	if err != nil {
		t.Fatalf("Failed to create update helper script: %v", err)
	}
	defer os.Remove(scriptPath)

	// Verify script contains error handling for non-existent files
	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("Failed to read script content: %v", err)
	}

	scriptStr := string(scriptContent)
	if !strings.Contains(scriptStr, "New binary file does not exist") {
		t.Errorf("Script missing error handling for non-existent new binary")
	}
}
