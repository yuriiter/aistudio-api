package services

import (
	"encoding/json"
	"fmt"
	"log" // Added for timestamped logging
	"os"
	"strings"
	"sync"
	"time"

	"aistudio-api/openai"
	"github.com/playwright-community/playwright-go"
)

// Selectors
const (
	SELECTOR_TEXTAREA      = "textarea"
	SELECTOR_ADD_MEDIA_BTN = "ms-add-media-button button"
	SELECTOR_UPLOAD_TEXT   = "Upload files"

	// Idle = Ready to click (Type="submit")
	SELECTOR_RUN_IDLE = "ms-run-button button[type='submit']"
	// Busy = Generating (Type="button")
	SELECTOR_RUN_BUSY = "ms-run-button button[type='button']"
)

// Configuration
const (
	MAX_RETRIES = 3
	RETRY_DELAY = 2 * time.Second

	// Increased for safety
	PARSE_ATTEMPTS = 5
	PARSE_INTERVAL = 200 * time.Millisecond
)

// ExecuteChatInteraction handles the full retry loop
func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
	var lastErr error

	for i := 0; i < MAX_RETRIES; i++ {
		if i > 0 {
			log.Printf(">> [Retry] Attempt %d/%d starting in %v...\n", i+1, MAX_RETRIES, RETRY_DELAY)
			time.Sleep(RETRY_DELAY)
		}

		log.Printf(">> [Attempt %d] Starting interaction...\n", i+1)
		response, err := executeAttempt(req)
		if err == nil {
			return response, nil
		}

		log.Printf(">> [Attempt %d] Failed: %v\n", i+1, err)
		lastErr = err
	}

	return "", fmt.Errorf("failed after %d attempts. Last error: %v", MAX_RETRIES, lastErr)
}

// executeAttempt contains the Playwright logic
func executeAttempt(req openai.ChatCompletionRequest) (string, error) {
	page, err := Manager.CreateNewPage()
	if err != nil {
		return "", err
	}
	defer page.Close()

	// ---------------------------------------------------------
	// 1. Setup Network Listener with Signal Channel
	// ---------------------------------------------------------
	var capturedBodies [][]byte
	var captureMu sync.Mutex

	// Channel to signal that a generation request has finished downloading
	networkFinishedChan := make(chan bool, 1)

	page.OnResponse(func(response playwright.Response) {
		if strings.Contains(response.URL(), "GenerateContent") {
			log.Println(">> [Net] Detected 'GenerateContent' stream...")

			// Processing body in a goroutine to ensure we don't block the event loop,
			// though Body() itself blocks until stream close.
			go func() {
				// This waits until the server closes the stream
				body, err := response.Body()
				if err != nil {
					log.Printf("!! [Net] Error reading body: %v\n", err)
					return
				}

				log.Printf(">> [Net] Stream closed. Captured %d bytes.\n", len(body))

				captureMu.Lock()
				capturedBodies = append(capturedBodies, body)
				captureMu.Unlock()

				// Signal that we have data
				select {
				case networkFinishedChan <- true:
				default:
				}
			}()
		}
	})

	// ---------------------------------------------------------
	// 2. Navigation
	// ---------------------------------------------------------
	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}

	log.Println(">> Navigating to AI Studio...")
	if _, err := page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(30000)}); err != nil {
		return "", fmt.Errorf("navigation failed: %v", err)
	}

	// ---------------------------------------------------------
	// 3. File Upload
	// ---------------------------------------------------------
	log.Println(">> Preparing prompt file...")
	fullPrompt := buildPromptFromMessages(req.Messages)
	tmpFile, err := os.CreateTemp("", "aistudio-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(fullPrompt); err != nil {
		return "", fmt.Errorf("failed to write to temp file: %v", err)
	}
	tmpFilePath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpFilePath)

	// Open Media Menu
	addMediaBtn := page.Locator(SELECTOR_ADD_MEDIA_BTN).First()
	if err := addMediaBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("add media button not found")
	}
	addMediaBtn.Click()

	// Handle File Chooser
	uploadBtn := page.GetByText(SELECTOR_UPLOAD_TEXT).First()
	if err := uploadBtn.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); err != nil {
		return "", fmt.Errorf("upload text not found")
	}
	// Small delay for animation stability
	time.Sleep(500 * time.Millisecond)

	fileChooser, err := page.ExpectFileChooser(func() error {
		return uploadBtn.Click()
	})
	if err != nil {
		return "", fmt.Errorf("file chooser failed: %v", err)
	}
	if err := fileChooser.SetFiles([]string{tmpFilePath}); err != nil {
		return "", fmt.Errorf("failed to set files: %v", err)
	}
	log.Println(">> File attached.")

	// ---------------------------------------------------------
	// 4. Run Execution
	// ---------------------------------------------------------
	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	time.Sleep(1 * time.Second) // Wait for attachment processing

	log.Println(">> Clicking Run...")
	if err := runBtn.Click(); err != nil {
		// Fallback
		page.Locator(SELECTOR_TEXTAREA).Press("Control+Enter")
	}

	// ---------------------------------------------------------
	// 5. Dual Wait Strategy (Network OR UI)
	// ---------------------------------------------------------

	// Step A: Wait for it to START (Busy button appears)
	log.Println(">> Waiting for UI to switch to BUSY...")
	busyBtn := page.Locator(SELECTOR_RUN_BUSY).First()
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(15000),
	})
	if err != nil {
		// If we missed the busy state but network finished, that's fine too.
		// We check network below.
		log.Printf("!! Warning: UI didn't show 'Stop' button (fast response?). Checking network...\n")
	} else {
		log.Println(">> UI is BUSY. Generation started.")
	}

	// Step B: Race - Wait for Network Finish OR UI Idle
	log.Println(">> Waiting for completion (Network Stream OR UI Idle)...")

	uiIdleChan := make(chan error, 1)
	go func() {
		// Wait for the "Stop" button to disappear
		err := busyBtn.WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateHidden,
			Timeout: playwright.Float(180000), // 3 min max
		})
		uiIdleChan <- err
	}()

	select {
	case <-networkFinishedChan:
		log.Println(">> [Event] Network stream finished! (Bypassing UI wait)")
	case err := <-uiIdleChan:
		if err != nil {
			log.Printf("!! [Event] UI Wait timed out: %v\n", err)
		} else {
			log.Println(">> [Event] UI became IDLE.")
		}
	case <-time.After(180 * time.Second):
		return "", fmt.Errorf("global timeout waiting for response")
	}

	// ---------------------------------------------------------
	// 6. Result Extraction
	// ---------------------------------------------------------
	log.Println(">> Processing captured data...")

	// We try a few times in case the network handler is finalizing bytes
	for i := 0; i < PARSE_ATTEMPTS; i++ {
		captureMu.Lock()
		currentData := make([][]byte, len(capturedBodies))
		copy(currentData, capturedBodies)
		captureMu.Unlock()

		if len(currentData) > 0 {
			fullText, err := parseAllChunks(currentData)
			if err == nil && len(strings.TrimSpace(fullText)) > 0 {
				log.Printf(">> Success! Extracted %d chars.\n", len(fullText))
				return fullText, nil
			}
		}

		log.Printf("   [Poll %d] Data not ready or empty. Retrying...\n", i+1)
		time.Sleep(PARSE_INTERVAL)
	}

	return "", fmt.Errorf("no valid text extracted from %d network chunks", len(capturedBodies))
}

func buildPromptFromMessages(msgs []openai.ChatCompletionMessage) string {
	var sb strings.Builder
	for _, msg := range msgs {
		role := strings.ToUpper(msg.Role)
		sb.WriteString(fmt.Sprintf("%s: %s\n\n", role, msg.Content))
	}
	sb.WriteString("ASSISTANT: ")
	return sb.String()
}

func parseAllChunks(bodies [][]byte) (string, error) {
	var allTextSegments []string

	for _, body := range bodies {
		chunkText, err := parseSingleChunk(body)
		if err == nil && chunkText != "" {
			allTextSegments = append(allTextSegments, chunkText)
		}
	}

	if len(allTextSegments) == 0 {
		return "", fmt.Errorf("no valid text found")
	}

	return strings.Join(allTextSegments, ""), nil
}

func parseSingleChunk(body []byte) (string, error) {
	rawStr := string(body)

	// 1. Strip Google's XSSI prefix
	if strings.HasPrefix(rawStr, ")]}'") {
		rawStr = strings.TrimPrefix(rawStr, ")]}'")
		rawStr = strings.TrimSpace(rawStr)
	}

	var raw []interface{}
	err := json.Unmarshal([]byte(rawStr), &raw)

	// 2. JSON Fixer
	if err != nil {
		openCount := strings.Count(rawStr, "[")
		closeCount := strings.Count(rawStr, "]")
		if openCount > closeCount {
			missing := openCount - closeCount
			fixedStr := rawStr + strings.Repeat("]", missing)
			err = json.Unmarshal([]byte(fixedStr), &raw)
		}
	}

	if err != nil {
		return "", err
	}

	var textChunks []string
	findTextChunks(raw, &textChunks)
	return strings.Join(textChunks, ""), nil
}

func findTextChunks(data interface{}, chunks *[]string) {
	switch v := data.(type) {
	case []interface{}:
		if len(v) >= 2 {
			if v[0] == nil {
				if text, ok := v[1].(string); ok {
					if !strings.HasPrefix(text, "v1_") && text != "model" && len(text) > 0 {
						*chunks = append(*chunks, text)
					}
				}
			}
		}
		for _, item := range v {
			findTextChunks(item, chunks)
		}
	}
}
