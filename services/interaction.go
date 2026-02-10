package services

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"aistudio-api/openai"
	"github.com/playwright-community/playwright-go"
)

const (
	SELECTOR_TEXTAREA      = "textarea"
	SELECTOR_ADD_MEDIA_BTN = "ms-add-media-button button"
	SELECTOR_UPLOAD_TEXT   = "Upload files"

	SELECTOR_RUN_IDLE = "ms-run-button button[type='submit']"
	SELECTOR_RUN_BUSY = "ms-run-button button[type='button']"
)

const (
	MAX_RETRIES = 3
	RETRY_DELAY = 2 * time.Second

	PARSE_ATTEMPTS = 5
	PARSE_INTERVAL = 200 * time.Millisecond
)

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

func executeAttempt(req openai.ChatCompletionRequest) (string, error) {
	page, err := Manager.CreateNewPage()
	if err != nil {
		return "", err
	}
	defer page.Close()

	var capturedBodies [][]byte
	var captureMu sync.Mutex

	networkFinishedChan := make(chan bool, 1)

	page.OnResponse(func(response playwright.Response) {
		if strings.Contains(response.URL(), "GenerateContent") {
			log.Println(">> [Net] Detected 'GenerateContent' stream...")

			go func() {
				body, err := response.Body()
				if err != nil {
					log.Printf("!! [Net] Error reading body: %v\n", err)
					return
				}

				log.Printf(">> [Net] Stream closed. Captured %d bytes.\n", len(body))

				captureMu.Lock()
				capturedBodies = append(capturedBodies, body)
				captureMu.Unlock()

				select {
				case networkFinishedChan <- true:
				default:
				}
			}()
		}
	})

	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}

	log.Println(">> Navigating to AI Studio...")
	if _, err := page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(30000)}); err != nil {
		return "", fmt.Errorf("navigation failed: %v", err)
	}

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

	addMediaBtn := page.Locator(SELECTOR_ADD_MEDIA_BTN).First()
	if err := addMediaBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("add media button not found")
	}
	addMediaBtn.Click()

	uploadBtn := page.GetByText(SELECTOR_UPLOAD_TEXT).First()
	if err := uploadBtn.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); err != nil {
		return "", fmt.Errorf("upload text not found")
	}
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

	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	time.Sleep(1 * time.Second)

	log.Println(">> Clicking Run...")
	if err := runBtn.Click(); err != nil {
		page.Locator(SELECTOR_TEXTAREA).Press("Control+Enter")
	}

	log.Println(">> Waiting for UI to switch to BUSY...")
	busyBtn := page.Locator(SELECTOR_RUN_BUSY).First()
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(15000),
	})
	if err != nil {
		log.Printf("!! Warning: UI didn't show 'Stop' button (fast response?). Checking network...\n")
	} else {
		log.Println(">> UI is BUSY. Generation started.")
	}

	log.Println(">> Waiting for completion (Network Stream OR UI Idle)...")

	uiIdleChan := make(chan error, 1)
	go func() {
		err := busyBtn.WaitFor(playwright.LocatorWaitForOptions{
			State:   playwright.WaitForSelectorStateHidden,
			Timeout: playwright.Float(180000),
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

	log.Println(">> Processing captured data...")

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
		role := strings.ToLower(msg.Role)
		if role == "system" {
			sb.WriteString(fmt.Sprintf("SYSTEM PROMPT: %s\n\n", msg.Content))
		} else {
			sb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.ToUpper(role), msg.Content))
		}
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

	if strings.HasPrefix(rawStr, ")]}'") {
		rawStr = strings.TrimPrefix(rawStr, ")]}'")
		rawStr = strings.TrimSpace(rawStr)
	}

	var raw []interface{}
	err := json.Unmarshal([]byte(rawStr), &raw)

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
