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

	SELECTOR_ASPECT_RATIO = `mat-select[aria-label="Aspect ratio"]`
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

func ExecuteImageGeneration(req openai.ImageRequest) ([]string, error) {
	var lastErr error

	for i := 0; i < MAX_RETRIES; i++ {
		if i > 0 {
			log.Printf(">> [Image Retry] Attempt %d/%d starting in %v...\n", i+1, MAX_RETRIES, RETRY_DELAY)
			time.Sleep(RETRY_DELAY)
		}

		log.Printf(">> [Image Attempt %d] Starting generation...\n", i+1)
		responses, err := executeImageAttempt(req)
		if err == nil && len(responses) > 0 {
			return responses, nil
		}

		log.Printf(">> [Image Attempt %d] Failed: %v\n", i+1, err)
		lastErr = err
	}

	return nil, fmt.Errorf("failed after %d attempts. Last error: %v", MAX_RETRIES, lastErr)
}

func executeImageAttempt(req openai.ImageRequest) ([]string, error) {
	page, err := Manager.CreateNewPage()
	if err != nil {
		return nil, err
	}
	defer page.Close()

	var capturedImages []string
	var captureMu sync.Mutex

	// Hook to network intercept the incoming Base64 image chunks immediately
	page.OnRequest(func(request playwright.Request) {
		url := request.URL()
		if strings.HasPrefix(url, "data:image/") {
			captureMu.Lock()
			capturedImages = append(capturedImages, url)
			captureMu.Unlock()
		}
	})

	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}

	log.Println(">> Navigating to AI Studio for Image Gen...")
	if _, err := page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(30000)}); err != nil {
		return nil, fmt.Errorf("navigation failed: %v", err)
	}

	time.Sleep(2 * time.Second) // Let SPA elements and options stabilize

	// Select Aspect Ratio mapping
	aspectRatio := mapSizeToAspectRatio(req.Size)
	arSelect := page.Locator(SELECTOR_ASPECT_RATIO).First()
	if err := arSelect.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(4000)}); err == nil {
		arSelect.Click()
		ratioOption := page.Locator("mat-option").GetByText(aspectRatio, playwright.LocatorGetByTextOptions{Exact: playwright.Bool(true)}).First()
		if err := ratioOption.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(2000)}); err == nil {
			ratioOption.Click()
			log.Printf(">> Aspect ratio set to %s\n", aspectRatio)
		} else {
			log.Printf(">> Warning: Aspect ratio option '%s' not found.\n", aspectRatio)
			page.Keyboard().Press("Escape") // Close dropdown
		}
	} else {
		log.Println(">> Warning: Aspect ratio selector not found. Proceeding with default.")
	}

	log.Println(">> Filling prompt...")
	textarea := page.Locator(SELECTOR_TEXTAREA).First()
	if err := textarea.WaitFor(); err != nil {
		return nil, fmt.Errorf("textarea not found")
	}
	if err := textarea.Fill(req.Prompt); err != nil {
		return nil, fmt.Errorf("failed to fill prompt: %v", err)
	}

	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return nil, fmt.Errorf("run button not found")
	}

	time.Sleep(1 * time.Second)

	captureMu.Lock()
	capturedImages = nil // Clear cache or avatar loads
	captureMu.Unlock()

	log.Println(">> Clicking Run...")
	if err := runBtn.Click(); err != nil {
		textarea.Press("Control+Enter")
	}

	log.Println(">> Waiting for UI to switch to BUSY...")
	busyBtn := page.Locator(SELECTOR_RUN_BUSY).First()
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(15000),
	})
	if err == nil {
		log.Println(">> UI is BUSY. Image generation started.")
	}

	log.Println(">> Waiting for completion...")
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateHidden,
		Timeout: playwright.Float(120000),
	})
	if err != nil {
		log.Printf("!! Warning: UI wait for hidden timed out: %v\n", err)
	} else {
		log.Println(">> [Event] UI became IDLE.")
	}

	time.Sleep(2 * time.Second) // Final yield to allow DOM renders & network events to complete

	captureMu.Lock()
	imgs := make([]string, len(capturedImages))
	copy(imgs, capturedImages)
	captureMu.Unlock()

	// Fallback DOM check if network missed the request (sometimes happens with dynamic elements)
	if len(imgs) == 0 {
		log.Println(">> No images in network requests, checking DOM...")
		evalResult, err := page.Evaluate(`() => {
			return Array.from(document.querySelectorAll('img'))
				.map(img => img.src)
				.filter(src => src.startsWith('data:image/'));
		}`)
		if err == nil && evalResult != nil {
			if arr, ok := evalResult.([]interface{}); ok {
				for _, v := range arr {
					if str, ok := v.(string); ok {
						imgs = append(imgs, str)
					}
				}
			}
		}
	}

	if len(imgs) == 0 {
		return nil, fmt.Errorf("no images found after generation")
	}

	// Deduplication and small icon filtering
	// Data URLs > 5000 characters ensures we omit purely small UI icons loaded onto the canvas
	uniqueImgs := make([]string, 0)
	seen := make(map[string]bool)
	for _, img := range imgs {
		if len(img) > 5000 && !seen[img] {
			seen[img] = true
			uniqueImgs = append(uniqueImgs, img)
		}
	}

	if len(uniqueImgs) == 0 {
		return nil, fmt.Errorf("found data images, but they were likely UI icons (too small)")
	}

	log.Printf(">> Successfully captured %d unique image(s).\n", len(uniqueImgs))

	// Honor the N limit (if available)
	n := req.N
	if len(uniqueImgs) > n {
		uniqueImgs = uniqueImgs[:n]
	}

	return uniqueImgs, nil
}

func mapSizeToAspectRatio(size string) string {
	if size == "" {
		return "1:1"
	}

	// Supported straight match by Studio UI dropdown
	validRatios := []string{"Auto", "1:1", "9:16", "16:9", "3:4", "4:3", "3:2", "2:3", "5:4", "4:5", "21:9"}
	for _, r := range validRatios {
		if size == r {
			return r
		}
	}

	// Map typical OpenAI dimensions -> Studio Native Option
	switch size {
	case "256x256", "512x512", "1024x1024":
		return "1:1"
	case "1024x1792":
		return "9:16"
	case "1792x1024":
		return "16:9"
	}

	// Default
	return "1:1"
}

// === EXISTING CHAT HELPERS ===

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
