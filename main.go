package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const (
	numChannels   = 1  // Mono audio
	bitsPerSample = 16 // 16 bits per sample
)

// CreateWAVHeader generates a WAV header for the given data length
func createWAVHeader(dataLength int, sampleRate int) []byte {
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	header := make([]byte, 44)

	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataLength))
	copy(header[8:12], []byte("WAVE"))

	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], bitsPerSample)

	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataLength))

	return header
}

func uploadFileToGCS(bucketName string, fileName string, filePath string) error {
	ctx := context.Background()

	// Create a storage client using the service account credentials from the environment variable
	credsEnv := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON")
	if credsEnv == "" {
		return fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS_JSON environment variable is not set")
	}

	// Decode the base64 encoded credentials
	creds, err := base64.StdEncoding.DecodeString(credsEnv)
	if err != nil {
		return fmt.Errorf("failed to decode credentials: %v", err)
	}

	// Create a temporary file for the credentials
	credsFile, err := os.CreateTemp("", "gcs-creds-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file for credentials: %v", err)
	}
	defer os.Remove(credsFile.Name())

	if _, err := credsFile.Write(creds); err != nil {
		return fmt.Errorf("failed to write credentials to temp file: %v", err)
	}
	credsFile.Close()

	// Create a storage client
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(credsFile.Name()))
	if err != nil {
		return fmt.Errorf("failed to create storage client: %v", err)
	}
	defer client.Close()

	bucket := client.Bucket(bucketName)
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer f.Close()

	wc := bucket.Object(fileName).NewWriter(ctx)
	wc.ContentType = "audio/wav"

	if _, err = io.Copy(wc, f); err != nil {
		return fmt.Errorf("failed to write to bucket: %v", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to close Writer: %v", err)
	}

	log.Printf("File %s uploaded to GCS bucket %s successfully.", fileName, bucketName)
	return nil
}

func handlePostAudio(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	sampleRateParam := query.Get("sample_rate")
	uid := query.Get("uid")

	log.Printf("Received request from uid: %s", uid)
	log.Printf("Requested sample rate: %s", sampleRateParam)

	// Parse the sample rate if provided
	var sampleRateValue int
	if sampleRateParam != "" {
		sampleRateValue, err := strconv.Atoi(sampleRateParam)
		if err != nil || sampleRateValue <= 0 {
			log.Printf("Invalid sample rate: %s, using default: %d Hz", sampleRateParam, 16000)
			sampleRateValue = 16000
		}
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	currentTime := time.Now()
	filename := fmt.Sprintf("%02d_%02d_%04d_%02d_%02d_%02d.wav",
		currentTime.Day(),
		currentTime.Month(),
		currentTime.Year(),
		currentTime.Hour(),
		currentTime.Minute(),
		currentTime.Second())

	tempFilePath := filepath.Join(os.TempDir(), filename)

	header := createWAVHeader(len(body), sampleRateValue)

	// Write to temporary file
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		log.Printf("Failed to create temp file: %v", err)
		http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	// Write WAV header and audio data
	tempFile.Write(header)
	tempFile.Write(body)

	// Get bucket name from environment variable
	bucketName := os.Getenv("GCS_BUCKET_NAME")
	if bucketName == "" {
		log.Printf("GCS_BUCKET_NAME environment variable is not set")
		http.Error(w, "GCS_BUCKET_NAME environment variable is not set", http.StatusInternalServerError)
		return
	}

	// Upload the file to Google Cloud Storage
	err = uploadFileToGCS(bucketName, filename, tempFilePath)
	if err != nil {
		log.Printf("Failed to upload to GCS: %v", err)
		http.Error(w, "Failed to upload to Google Cloud Storage", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Audio bytes received and uploaded as %s", filename)))
}

func main() {
	http.HandleFunc("/audio", handlePostAudio)
	port := "8080"
	log.Printf("Server starting on port %s...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
