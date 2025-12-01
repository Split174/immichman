package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

type ImmichClient struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

type AlbumResponse struct {
	ID        string `json:"id"`
	AlbumName string `json:"albumName"`
}

type CreateAlbumResponse struct {
	ID string `json:"id"`
}

// Ответ при загрузке
type UploadResponse struct {
	ID        string `json:"id"`
	Duplicate bool   `json:"duplicate"`
}

type UserResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func NewImmichClient(baseURL, apiKey string) *ImmichClient {
	cleanURL := strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(cleanURL, "/api") {
		cleanURL = strings.TrimSuffix(cleanURL, "/api")
	}
	return &ImmichClient{
		BaseURL: cleanURL,
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 300 * time.Second}, // Тайм-аут побольше
	}
}

func (ic *ImmichClient) Ping() error {
	url := fmt.Sprintf("%s/api/users/me", ic.BaseURL)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", ic.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ic.Client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка сети: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Immich Ping Error (Status %d): %s", resp.StatusCode, string(body))
	}

	var user UserResponse
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil // Успех, но не распарсили JSON (не критично)
	}
	log.Printf("Immich Connection OK. User: %s", user.Email)
	return nil
}

func (ic *ImmichClient) GetOrCreateAlbum(albumName string) (string, error) {
	targetURL := fmt.Sprintf("%s/api/albums", ic.BaseURL)

	// 1. Получаем список
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", ic.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ic.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var albums []AlbumResponse
		if err := json.NewDecoder(resp.Body).Decode(&albums); err == nil {
			for _, album := range albums {
				if album.AlbumName == albumName {
					return album.ID, nil
				}
			}
		}
	}

	// 2. Создаем
	log.Printf("Создаем альбом '%s'...", albumName)
	createBody := map[string]string{"albumName": albumName}
	jsonBody, _ := json.Marshal(createBody)

	req, err = http.NewRequest("POST", targetURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", ic.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err = ic.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ошибка создания альбома: %s", string(body))
	}

	var newAlbum CreateAlbumResponse
	if err := json.NewDecoder(resp.Body).Decode(&newAlbum); err != nil {
		return "", err
	}
	return newAlbum.ID, nil
}

// UploadAsset просто загружает файл
func (ic *ImmichClient) UploadAsset(fileName string, fileReader io.Reader, createdAt time.Time, deviceAssetID string) (*UploadResponse, error) {
	uploadURL := fmt.Sprintf("%s/api/assets", ic.BaseURL)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("assetData", fileName)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(part, fileReader)
	if err != nil {
		return nil, err
	}

	_ = writer.WriteField("deviceAssetId", deviceAssetID)
	_ = writer.WriteField("deviceId", "TELEGRAM-BOT")
	_ = writer.WriteField("fileCreatedAt", createdAt.Format(time.RFC3339))
	_ = writer.WriteField("fileModifiedAt", createdAt.Format(time.RFC3339))
	_ = writer.WriteField("isFavorite", "false")

	if err := writer.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", uploadURL, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", ic.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ic.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Status %d: %s", resp.StatusCode, string(respBody))
	}

	var uploadResp UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		// Если JSON не пришел, но статус ОК - создаем фальшивый ID (возможно дубликат без тела)
		return &UploadResponse{Duplicate: true, ID: ""}, nil
	}

	return &uploadResp, nil
}

// AddAssetToAlbum привязывает загруженный файл к альбому (PUT /api/albums/:id/assets)
func (ic *ImmichClient) AddAssetToAlbum(albumID string, assetID string) error {
	if assetID == "" {
		return fmt.Errorf("получен пустой assetID, невозможно добавить в альбом")
	}

	url := fmt.Sprintf("%s/api/albums/%s/assets", ic.BaseURL, albumID)

	// Тело запроса: { "ids": ["uuid-asset-id"] }
	payload := map[string][]string{
		"ids": {assetID},
	}
	jsonBody, _ := json.Marshal(payload)

	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", ic.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := ic.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ошибка добавления в альбом (код %d): %s", resp.StatusCode, string(body))
	}

	log.Printf("Успешно добавлено в альбом %s (Asset: %s)", albumID, assetID)
	return nil
}
