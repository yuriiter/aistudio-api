package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"aistudio-api/openai"
	"aistudio-api/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func HandleChatCompletions(c *gin.Context) {
	var req openai.ChatCompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	responseText, err := services.ExecuteChatInteraction(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, openai.APIError{
			Error: openai.APIErrorDetail{
				Message: err.Error(),
				Type:    "server_error",
			},
		})
		return
	}

	// Rough estimate for tokens based on content format
	promptLength := 0
	if len(req.Messages) > 0 {
		promptLength = len(fmt.Sprint(req.Messages[0].Content))
	}

	resp := openai.ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: responseText,
				},
				FinishReason: "stop",
			},
		},
		Usage: openai.Usage{
			PromptTokens:     promptLength / 4,
			CompletionTokens: len(responseText) / 4,
			TotalTokens:      (promptLength + len(responseText)) / 4,
		},
	}

	c.JSON(http.StatusOK, resp)
}

func HandleImageGenerations(c *gin.Context) {
	var req openai.ImageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON"})
		return
	}

	// === SET DEFAULTS FOR IMAGE GENERATION ===
	if req.Model == "" {
		req.Model = "gemini-2.5-flash-image"
	}
	if req.N <= 0 {
		req.N = 1
	}
	if req.Size == "" {
		req.Size = "1024x1024"
	}

	b64Images, err := services.ExecuteImageGeneration(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, openai.APIError{
			Error: openai.APIErrorDetail{
				Message: err.Error(),
				Type:    "server_error",
			},
		})
		return
	}

	resp := openai.ImageResponse{
		Created: time.Now().Unix(),
		Data:    []openai.ImageData{},
	}

	for _, b64 := range b64Images {
		data := openai.ImageData{}
		cleanB64 := b64

		// Extract raw base64 if needed, fallback for b64_json request
		if strings.Contains(b64, ",") {
			cleanB64 = strings.SplitN(b64, ",", 2)[1]
		}

		if req.ResponseFormat == "b64_json" {
			data.B64JSON = cleanB64
		} else {
			// By default (or "url"), we return the data URI string itself
			data.URL = b64
		}
		resp.Data = append(resp.Data, data)
	}

	c.JSON(http.StatusOK, resp)
}
