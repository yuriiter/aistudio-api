package main

import (
	"net/http"
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

	// Call the automation service
	responseText, err := services.ExecuteChatInteraction(req)
	if err != nil {
		// Return OpenAI formatted error
		c.JSON(http.StatusInternalServerError, openai.APIError{
			Error: openai.APIErrorDetail{
				Message: err.Error(),
				Type:    "server_error",
			},
		})
		return
	}

	// Construct OpenAI Response Object
	resp := openai.ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model, // Echo back the model request
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
			// We can't easily count tokens without a tokenizer library,
			// so we use dummy values or estimate based on string length (char / 4).
			PromptTokens:     len(req.Messages[0].Content) / 4,
			CompletionTokens: len(responseText) / 4,
			TotalTokens:      (len(req.Messages[0].Content) + len(responseText)) / 4,
		},
	}

	c.JSON(http.StatusOK, resp)
}
