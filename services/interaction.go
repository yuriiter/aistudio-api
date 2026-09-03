package services

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"aistudio-api/openai"
	"github.com/mxschmitt/playwright-go"
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

	PARSE_ATTEMPTS = 5
	PARSE_INTERVAL = 200 * time.Millisecond
)

// === CORE WORKFLOWS ===

func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
	var lastErr error

	for i := 0; i < MAX_RETRIES; i++ {
		if i > 0 {
			delay := retryDelay()
			log.Printf(">> [Retry] Attempt %d/%d starting in %v...\n", i+1, MAX_RETRIES, delay)
			time.Sleep(delay)
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
	page, err := Manager.AcquireRunPage()
	if err != nil {
		return "", err
	}
	defer Manager.ReleaseRunPage()

	// Reset capture bucket for this run.
	capture.mu.Lock()
	capture.bodies = nil
	capture.done = make(chan bool, 1)
	capture.mu.Unlock()

	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}

	log.Println(">> Navigating to AI Studio...")
	if _, err := page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(30000)}); err != nil {
		return "", fmt.Errorf("navigation failed: %v", err)
	}

	warmUpPage(page)

	// 1. Process Messages (Text + Media)
	log.Println(">> Parsing prompt and attachments...")
	fullPrompt, attachedFiles := parseChatMessages(req.Messages)

	// 2. Prepare master text file
	tmpFile, err := os.CreateTemp("", "aistudio-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	if _, err := tmpFile.WriteString(fullPrompt); err != nil {
		return "", fmt.Errorf("failed to write to temp file: %v", err)
	}
	tmpFilePath := tmpFile.Name()
	tmpFile.Close()

	// Compile all files to upload
	allUploadFiles := append([]string{tmpFilePath}, attachedFiles...)

	// Cleanup all temp files later
	defer func() {
		for _, file := range allUploadFiles {
			os.Remove(file)
		}
	}()

	// 3. Upload files
	if err := uploadFiles(page, allUploadFiles); err != nil {
		return "", err
	}

	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	time.Sleep(2 * time.Second) // Let file uploads finish processing on UI

	humanPause()

	log.Println(">> Clicking Run...")
	if err := humanClick(page, runBtn); err != nil {
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
	case <-capture.done:
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
		capture.mu.Lock()
		currentData := make([][]byte, len(capture.bodies))
		copy(currentData, capture.bodies)
		capture.mu.Unlock()

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

	capture.mu.Lock()
	n := len(capture.bodies)
	capture.mu.Unlock()
	return "", fmt.Errorf("no valid text extracted from %d network chunks", n)
}

func ExecuteImageGeneration(req openai.ImageRequest) ([]string, error) {
	var lastErr error

	for i := 0; i < MAX_RETRIES; i++ {
		if i > 0 {
			delay := retryDelay()
			log.Printf(">> [Image Retry] Attempt %d/%d starting in %v...\n", i+1, MAX_RETRIES, delay)
			time.Sleep(delay)
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
	page, err := Manager.AcquireRunPage()
	if err != nil {
		return nil, err
	}
	defer Manager.ReleaseRunPage()

	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}

	log.Println(">> Navigating to AI Studio for Image Gen...")
	if _, err := page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(30000)}); err != nil {
		return nil, fmt.Errorf("navigation failed: %v", err)
	}

	warmUpPage(page)

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

	// Extract and prepare input files (images)
	var attachedFiles []string
	if req.Image != "" && strings.HasPrefix(req.Image, "data:") {
		if path, err := writeDataURIToTempFile(req.Image); err == nil {
			attachedFiles = append(attachedFiles, path)
		}
	}
	for _, fUri := range req.Files {
		if strings.HasPrefix(fUri, "data:") {
			if path, err := writeDataURIToTempFile(fUri); err == nil {
				attachedFiles = append(attachedFiles, path)
			}
		}
	}

	defer func() {
		for _, f := range attachedFiles {
			os.Remove(f)
		}
	}()

	if len(attachedFiles) > 0 {
		log.Printf(">> Uploading %d input files...\n", len(attachedFiles))
		if err := uploadFiles(page, attachedFiles); err != nil {
			return nil, err
		}
		time.Sleep(2 * time.Second) // Give it time to attach to the UI
	}

	log.Println(">> Typing prompt...")
	textarea := page.Locator(SELECTOR_TEXTAREA).First()
	if err := textarea.WaitFor(); err != nil {
		return nil, fmt.Errorf("textarea not found")
	}
	if err := humanType(textarea, req.Prompt); err != nil {
		return nil, fmt.Errorf("failed to type prompt: %v", err)
	}

	humanPause()

	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return nil, fmt.Errorf("run button not found")
	}

	time.Sleep(1 * time.Second)
	humanPause()

	// --- BEFORE RUN: Capture snapshot of existing images (Thumbnails) ---
	beforeImages := getImagesFromDOM(page)
	log.Printf(">> Captured %d existing images in DOM before generation.\n", len(beforeImages))

	log.Println(">> Clicking Run...")
	if err := humanClick(page, runBtn); err != nil {
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

	time.Sleep(2 * time.Second) // Final yield to allow DOM renders to complete

	// --- AFTER RUN: Extract new images ---
	afterImages := getImagesFromDOM(page)

	uniqueImgs := make([]string, 0)
	for imgStr := range afterImages {
		if !beforeImages[imgStr] && len(imgStr) > 5000 {
			uniqueImgs = append(uniqueImgs, imgStr)
		}
	}

	if len(uniqueImgs) == 0 {
		return nil, fmt.Errorf("no newly generated images found (found %d total images, but they were all present before generation or too small)", len(afterImages))
	}

	log.Printf(">> Successfully captured %d unique image(s).\n", len(uniqueImgs))

	n := req.N
	if len(uniqueImgs) > n {
		uniqueImgs = uniqueImgs[:n]
	}

	return uniqueImgs, nil
}

// === UTILITIES ===

// humanPause waits a human-like random interval between discrete UI actions.
func humanPause() {
	delay := 400 + rand.Intn(1200)
	time.Sleep(time.Duration(delay) * time.Millisecond)
}

// retryDelay returns a long, jittered backoff so repeated failures don't hammer
// the GenerateContent endpoint (instant retries are a classic abuse signal).
func retryDelay() time.Duration {
	delay := 12000 + rand.Intn(18000)
	return time.Duration(delay) * time.Millisecond
}

// warmUpPage spends a few seconds moving the mouse and scrolling like a person
// settling into the page. This accumulates the engagement signals Google's
// risk-scoring uses before the GenerateContent request is fired.
func warmUpPage(page playwright.Page) {
	steps := 3 + rand.Intn(3)
	for i := 0; i < steps; i++ {
		x := float64(200 + rand.Intn(1200))
		y := float64(200 + rand.Intn(600))
		if err := page.Mouse().Move(x, y, playwright.MouseMoveOptions{Steps: playwright.Int(15 + rand.Intn(25))}); err != nil {
			break
		}
		time.Sleep(time.Duration(150+rand.Intn(350)) * time.Millisecond)
	}
	// A light scroll, like scanning the page.
	if err := page.Mouse().Wheel(0, float64(60+rand.Intn(120))); err == nil {
		time.Sleep(300 * time.Millisecond)
		page.Mouse().Wheel(0, 0)
	}
	humanPause()
}

// humanClick clicks a locator the way a person would: eases the cursor to the
// target with several interpolated moves, then presses and releases with a
// pause in between (no teleport, no instantaneous 2ms click).
func humanClick(page playwright.Page, locator playwright.Locator) error {
	if err := locator.ScrollIntoViewIfNeeded(); err != nil {
		return err
	}

	box, err := locator.BoundingBox()
	if err != nil {
		log.Printf("!! humanClick: bounding box failed: %v\n", err)
		return locator.Click()
	}

	// Aim slightly off-center, like a person.
	jitterX := (rand.Float64()*2 - 1) * box.Width * 0.2
	jitterY := (rand.Float64()*2 - 1) * box.Height * 0.2
	targetX := box.X + box.Width/2 + jitterX
	targetY := box.Y + box.Height/2 + jitterY

	// A few approach moves before landing on the target.
	waypoints := 2 + rand.Intn(3)
	for i := 1; i <= waypoints; i++ {
		t := float64(i) / float64(waypoints+1)
		wx := box.X + (targetX-box.X)*t + (rand.Float64()*2-1)*40
		wy := box.Y + (targetY-box.Y)*t + (rand.Float64()*2-1)*40
		if err := page.Mouse().Move(wx, wy, playwright.MouseMoveOptions{Steps: playwright.Int(10 + rand.Intn(15))}); err != nil {
			return err
		}
		time.Sleep(time.Duration(60+rand.Intn(180)) * time.Millisecond)
	}

	if err := page.Mouse().Move(targetX, targetY, playwright.MouseMoveOptions{Steps: playwright.Int(8)}); err != nil {
		return err
	}
	time.Sleep(time.Duration(120+rand.Intn(240)) * time.Millisecond)

	if err := page.Mouse().Down(); err != nil {
		return err
	}
	time.Sleep(time.Duration(80+rand.Intn(160)) * time.Millisecond)
	if err := page.Mouse().Up(); err != nil {
		return err
	}
	humanPause()
	return nil
}

// humanType types text like a person: focuses the field, clears it, then sends
// real per-key events with randomized inter-key delays. This avoids the
// instant single-input-event `Fill` which AI Studio treats as an automated bot.
func humanType(locator playwright.Locator, text string) error {
	if err := locator.Click(); err != nil {
		return err
	}

	if err := locator.Press("Control+A"); err != nil {
		return err
	}
	if err := locator.Press("Backspace"); err != nil {
		return err
	}

	humanPause()

	delay := 40 + rand.Float64()*80
	if err := locator.PressSequentially(text, playwright.LocatorPressSequentiallyOptions{
		Delay: playwright.Float(delay),
	}); err != nil {
		return err
	}

	humanPause()
	return nil
}

func getImagesFromDOM(page playwright.Page) map[string]bool {
	// Evaluates the DOM. Converts both standard base64 data URIs and object Blobs to Data URIs.
	script := `async () => {
		const imgs = Array.from(document.querySelectorAll('img'));
		const results = [];
		for (const img of imgs) {
			const src = img.src;
			if (!src) continue;
			if (src.startsWith('data:image/')) {
				results.push(src);
			} else if (src.startsWith('blob:')) {
				try {
					const response = await fetch(src);
					const blob = await response.blob();
					const reader = new FileReader();
					const b64 = await new Promise(resolve => {
						reader.onloadend = () => resolve(reader.result);
						reader.readAsDataURL(blob);
					});
					results.push(b64);
				} catch (e) {
					console.error("Failed to read blob:", e);
				}
			}
		}
		return results;
	}`

	imagesMap := make(map[string]bool)
	evalResult, err := page.Evaluate(script)
	if err == nil && evalResult != nil {
		if arr, ok := evalResult.([]interface{}); ok {
			for _, v := range arr {
				if str, ok := v.(string); ok {
					imagesMap[str] = true
				}
			}
		}
	}
	return imagesMap
}

func uploadFiles(page playwright.Page, files []string) error {
	if len(files) == 0 {
		return nil
	}
	addMediaBtn := page.Locator(SELECTOR_ADD_MEDIA_BTN).First()
	if err := addMediaBtn.WaitFor(); err != nil {
		return fmt.Errorf("add media button not found")
	}
	addMediaBtn.Click()

	uploadBtn := page.GetByText(SELECTOR_UPLOAD_TEXT).First()
	if err := uploadBtn.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible}); err != nil {
		return fmt.Errorf("upload text not found")
	}
	time.Sleep(500 * time.Millisecond)

	fileChooser, err := page.ExpectFileChooser(func() error {
		return uploadBtn.Click()
	})
	if err != nil {
		return fmt.Errorf("file chooser failed: %v", err)
	}

	if err := fileChooser.SetFiles(files); err != nil {
		return fmt.Errorf("failed to set files: %v", err)
	}
	log.Println(">> Files attached successfully.")
	return nil
}

func parseChatMessages(msgs []openai.ChatCompletionMessage) (string, []string) {
	var sb strings.Builder
	var files []string

	for _, msg := range msgs {
		role := strings.ToUpper(msg.Role)
		if strings.ToLower(msg.Role) == "system" {
			role = "SYSTEM PROMPT"
		}

		switch v := msg.Content.(type) {
		case string:
			sb.WriteString(fmt.Sprintf("%s: %s\n\n", role, v))
		case []interface{}:
			sb.WriteString(fmt.Sprintf("%s: ", role))
			for _, itemInterface := range v {
				item, ok := itemInterface.(map[string]interface{})
				if !ok {
					continue
				}
				typeStr, _ := item["type"].(string)

				if typeStr == "text" {
					textStr, _ := item["text"].(string)
					sb.WriteString(textStr + "\n")
				} else if typeStr == "image_url" || typeStr == "file_url" {
					urlMap, ok := item[typeStr].(map[string]interface{})
					if !ok {
						continue
					}
					urlStr, _ := urlMap["url"].(string)

					if strings.HasPrefix(urlStr, "data:") {
						path, err := writeDataURIToTempFile(urlStr)
						if err == nil {
							files = append(files, path)
							sb.WriteString(fmt.Sprintf("[Attached File]\n"))
						} else {
							log.Printf("Failed to process data URI: %v", err)
						}
					} else {
						sb.WriteString(fmt.Sprintf("[External File URL: %s]\n", urlStr))
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("ASSISTANT: ")
	return sb.String(), files
}

func writeDataURIToTempFile(dataURI string) (string, error) {
	parts := strings.SplitN(dataURI, ",", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid data URI")
	}

	mime := strings.TrimPrefix(parts[0], "data:")
	mime = strings.TrimSuffix(mime, ";base64")

	ext := ".bin"
	if strings.Contains(mime, "jpeg") || strings.Contains(mime, "jpg") {
		ext = ".jpg"
	} else if strings.Contains(mime, "png") {
		ext = ".png"
	} else if strings.Contains(mime, "webp") {
		ext = ".webp"
	} else if strings.Contains(mime, "pdf") {
		ext = ".pdf"
	} else if strings.Contains(mime, "csv") {
		ext = ".csv"
	} else if strings.Contains(mime, "text/plain") {
		ext = ".txt"
	}

	data, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}

	tmpFile, err := os.CreateTemp("", "aistudio-upload-*"+ext)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	if _, err = tmpFile.Write(data); err != nil {
		return "", err
	}
	return tmpFile.Name(), nil
}

func mapSizeToAspectRatio(size string) string {
	if size == "" {
		return "1:1"
	}

	validRatios := []string{"Auto", "1:1", "9:16", "16:9", "3:4", "4:3", "3:2", "2:3", "5:4", "4:5", "21:9"}
	for _, r := range validRatios {
		if size == r {
			return r
		}
	}

	switch size {
	case "256x256", "512x512", "1024x1024":
		return "1:1"
	case "1024x1792":
		return "9:16"
	case "1792x1024":
		return "16:9"
	}

	return "1:1"
}

// === EXISTING PARSING CHUNKS ===

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
