package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"aistudio-api/openai"
	"github.com/playwright-community/playwright-go"
)

// Selectors
const (
	SELECTOR_TEXTAREA = "textarea"
	// Idle = Ready to click (Generation finished)
	SELECTOR_RUN_IDLE = "button[aria-label='Run'][type='submit']"
	// Busy = Generating (Wait)
	SELECTOR_RUN_BUSY = "button[aria-label='Run'][type='button']"
)

// ResponseCapture holds the captured network response
type ResponseCapture struct {
	mu       sync.Mutex
	chunks   []string
	complete bool
}

// Advanced JavaScript spy that handles the response directly
const NETWORK_SPY_SCRIPT = `
(() => {
	// Intercept XMLHttpRequest
	const originalXHROpen = XMLHttpRequest.prototype.open;
	const originalXHRSend = XMLHttpRequest.prototype.send;
	
	window._AI_ACCUMULATOR = ""; 
	window._AI_CAPTURE_DONE = false;
	window._AI_CAPTURE_STARTED = false;

	XMLHttpRequest.prototype.open = function(method, url, ...args) {
		this._url = url;
		return originalXHROpen.call(this, method, url, ...args);
	};

	XMLHttpRequest.prototype.send = function(...args) {
		if (this._url && this._url.includes('GenerateContent')) {
			console.log('[Spy] Intercepting XHR GenerateContent');
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = "";
			window._AI_CAPTURE_DONE = false;

			// Attach listener BEFORE send
			const xhr = this;
			let lastLength = 0;
			
			this.addEventListener('readystatechange', function() {
				console.log('[Spy] ReadyState:', this.readyState, 'Status:', this.status);
				
				// State 3 = LOADING (streaming data)
				if (this.readyState === 3) {
					try {
						const currentText = this.responseText || "";
						if (currentText.length > lastLength) {
							window._AI_ACCUMULATOR = currentText;
							lastLength = currentText.length;
							console.log('[Spy] Streaming... accumulated', lastLength, 'bytes');
						}
					} catch (e) {
						console.log('[Spy] Error reading responseText in state 3:', e);
					}
				}
				
				// State 4 = DONE
				if (this.readyState === 4) {
					console.log('[Spy] XHR Complete, status:', this.status);
					console.log('[Spy] Response length:', this.responseText ? this.responseText.length : 0);
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
					console.log('[Spy] Captured', window._AI_ACCUMULATOR.length, 'bytes');
				}
				
				// State 0 = UNSENT/ABORTED - if we have data, mark as done
				if (this.readyState === 0 && window._AI_ACCUMULATOR.length > 0) {
					console.log('[Spy] XHR aborted, but we have', window._AI_ACCUMULATOR.length, 'bytes');
					window._AI_CAPTURE_DONE = true;
				}
			});
			
			// Also try using onload as backup
			this.addEventListener('load', function() {
				console.log('[Spy] XHR Load event fired');
				if (!window._AI_CAPTURE_DONE) {
					window._AI_ACCUMULATOR = this.responseText || "";
					window._AI_CAPTURE_DONE = true;
					console.log('[Spy] Captured via load event:', window._AI_ACCUMULATOR.length, 'bytes');
				}
			});
			
			// Handle errors
			this.addEventListener('error', function() {
				console.log('[Spy] XHR Error event');
				window._AI_CAPTURE_DONE = true;
			});
		}
		return originalXHRSend.apply(this, args);
	};

	// Also intercept fetch as backup
	const originalFetch = window.fetch;
	window.fetch = async (...args) => {
		const response = await originalFetch(...args);
		const url = args[0] instanceof Request ? args[0].url : args[0];

		if (url && url.includes("GenerateContent")) {
			console.log("[Spy] Intercepting Fetch GenerateContent");
			window._AI_CAPTURE_STARTED = true;
			window._AI_ACCUMULATOR = ""; 
			window._AI_CAPTURE_DONE = false;
			
			const clone = response.clone();
			const text = await clone.text();
			
			window._AI_ACCUMULATOR = text;
			window._AI_CAPTURE_DONE = true;
			console.log('[Spy] Captured', text.length, 'bytes via fetch');
		}
		return response;
	};
	
	console.log("[Spy] Network interceptors installed (XHR + Fetch)");
})();
`

func ExecuteChatInteraction(req openai.ChatCompletionRequest) (string, error) {
	page, err := Manager.CreateNewPage()
	if err != nil {
		return "", err
	}
	defer page.Close()

	// Enable console logging from the page
	page.OnConsole(func(msg playwright.ConsoleMessage) {
		fmt.Printf(">> [Browser Console] %s: %s\n", msg.Type(), msg.Text())
	})

	// Inject the spy script
	fmt.Println(">> Injecting network spy (XHR + Fetch)...")
	if err := page.AddInitScript(playwright.Script{Content: playwright.String(NETWORK_SPY_SCRIPT)}); err != nil {
		return "", fmt.Errorf("spy inject failed: %v", err)
	}

	// Navigate
	targetURL := NEW_PROMPT_PAGE
	if req.Model != "" {
		targetURL = fmt.Sprintf("%s?model=%s", NEW_PROMPT_PAGE, req.Model)
	}
	if _, err := page.Goto(targetURL); err != nil {
		return "", err
	}

	// Input
	fmt.Println(">> Waiting for input...")
	inputLocator := page.Locator(SELECTOR_TEXTAREA).First()
	if err := inputLocator.WaitFor(); err != nil {
		return "", fmt.Errorf("input not found")
	}

	fullPrompt := buildPromptFromMessages(req.Messages)
	if err := inputLocator.Fill(fullPrompt); err != nil {
		return "", err
	}

	// Click Run (IDLE state)
	fmt.Println(">> Clicking Run...")
	runBtn := page.Locator(SELECTOR_RUN_IDLE).First()
	if err := runBtn.WaitFor(); err != nil {
		return "", fmt.Errorf("run button not found")
	}

	if err := runBtn.Click(); err != nil {
		inputLocator.Press("Control+Enter")
	}

	// Wait for UI to become BUSY (generation started)
	fmt.Println(">> Waiting for generation to START...")
	busyBtn := page.Locator(SELECTOR_RUN_BUSY).First()
	err = busyBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(10000),
	})
	if err != nil {
		return "", fmt.Errorf("UI failed to switch to 'generating' mode")
	}

	// Give capture a moment to start
	time.Sleep(500 * time.Millisecond)

	// Verify capture started
	started, _ := page.Evaluate("window._AI_CAPTURE_STARTED")
	if isStarted, ok := started.(bool); !ok || !isStarted {
		fmt.Println(">> WARNING: Capture may not have started")
	} else {
		fmt.Println(">> Network capture confirmed started")
	}

	// Wait for UI to become IDLE (generation finished)
	fmt.Println(">> Generating... Waiting for completion (Button restore)...")
	err = runBtn.WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(120000), // Wait up to 2 mins
	})
	if err != nil {
		return "", fmt.Errorf("timeout waiting for AI to finish")
	}

	// Wait for the capture to complete
	fmt.Println(">> UI says done. Waiting for network capture to complete...")

	// Wait for capture done flag or timeout
	captureComplete := false
	for i := 0; i < 50; i++ { // 50 * 200ms = 10 seconds max
		done, err := page.Evaluate("window._AI_CAPTURE_DONE")
		if err == nil {
			if isDone, ok := done.(bool); ok && isDone {
				captureComplete = true
				fmt.Println(">> Network capture confirmed complete")
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !captureComplete {
		fmt.Println(">> WARNING: Capture done flag not set, reading anyway...")
	}

	// Give a bit more time for any final chunks to arrive
	time.Sleep(1 * time.Second)

	// Read captured data
	val, err := page.Evaluate("window._AI_ACCUMULATOR")
	if err != nil {
		return "", fmt.Errorf("failed to read spy data: %v", err)
	}

	rawText, ok := val.(string)
	if !ok || rawText == "" {
		// Debug info
		started, _ := page.Evaluate("window._AI_CAPTURE_STARTED")
		done, _ := page.Evaluate("window._AI_CAPTURE_DONE")
		fmt.Printf(">> DEBUG: Capture started=%v, done=%v\n", started, done)

		return "", fmt.Errorf("captured network data was empty (length: 0)")
	}

	fmt.Printf(">> Successfully captured %d bytes from network\n", len(rawText))

	// Optional: Print preview for debugging
	if len(rawText) > 0 && len(rawText) < 500 {
		fmt.Printf(">> Preview: %s\n", rawText)
	} else if len(rawText) >= 500 {
		fmt.Printf(">> Preview (first 300 chars): %s...\n", rawText[:300])
	}

	return parseGoogleRPCResponse([]byte(rawText))
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

func parseGoogleRPCResponse(body []byte) (string, error) {
	rawStr := string(body)

	// 1. Strip Google's Anti-Hijack Prefix if present
	if idx := strings.Index(rawStr, "["); idx != -1 {
		rawStr = rawStr[idx:]
	} else {
		// Sometimes it might just be the array, but if not found, we fail
		return "", fmt.Errorf("invalid JSON structure - no array found")
	}

	// 2. Unmarshal
	var raw []interface{}
	if err := json.Unmarshal([]byte(rawStr), &raw); err != nil {
		return "", fmt.Errorf("json parse error: %v (Length: %d)", err, len(rawStr))
	}

	// 3. Find the response text
	text := findLongestText(raw)
	if text == "" {
		return "", fmt.Errorf("parsed JSON but found no valid text content")
	}

	return text, nil
}

// findLongestText performs a DFS on the nested array to find the actual response string
func findLongestText(data interface{}) string {
	var allTexts []string
	collectTexts(data, &allTexts)

	// Filter and find the best response
	var longest string
	for _, text := range allTexts {
		// Skip metadata
		if strings.HasPrefix(text, "v1_") || text == "model" || len(text) < 2 {
			continue
		}

		// Skip "Drafting" thoughts and similar internal reasoning
		lower := strings.ToLower(text)
		if strings.Contains(lower, "drafting") ||
			strings.Contains(lower, "i've decided") ||
			strings.Contains(lower, "i'm going to") {
			continue
		}

		// Prefer content that looks like actual output
		if len(text) > len(longest) {
			longest = text
		}
	}

	return longest
}

// collectTexts recursively collects all string values from nested structure
func collectTexts(data interface{}, texts *[]string) {
	switch v := data.(type) {
	case []interface{}:
		for _, item := range v {
			collectTexts(item, texts)
		}
	case string:
		if len(v) >= 2 {
			*texts = append(*texts, v)
		}
	}
}
