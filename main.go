package function

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const (
	numChannels      = 1    // Mono audio
	sampleRate       = 16000
	bitsPerSample    = 16   // 16 bits per sample
	maxDuration      = 5 * time.Minute
	inactivityLimit  = 2 * time.Minute
	metadataFile     = "current_wav_metadata.json"
)

type WAVMetadata struct {
	Filename      string    `json:"filename"`
	LastWriteTime time.Time `json:"last_write_time"`
	CurrentSize   int       `json:"current_size"`
}

// calculateDuration returns the duration of audio based on size in bytes
func calculateDuration(sizeInBytes int) time.Duration {
	bytesPerSecond := sampleRate * numChannels * bitsPerSample / 8
	seconds := float64(sizeInBytes) / float64(bytesPerSecond)
	return time.Duration(seconds * float64(time.Second))
}

// getStorageClient creates a new Google Cloud Storage client
func getStorageClient(ctx context.Context) (*storage.Client, error) {
	credsEnv := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON")
	if credsEnv == "" {
		return nil, fmt.Errorf("GOOGLE_APPLICATION_CREDENTIALS_JSON environment variable is not set")
	}

	creds, err := base64.StdEncoding.DecodeString(credsEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to decode credentials: %v", err)
	}

	credsFile, err := os.CreateTemp("", "gcs-creds-*.json")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file for credentials: %v", err)
	}
	defer os.Remove(credsFile.Name())

	if _, err := credsFile.Write(creds); err != nil {
		return nil, fmt.Errorf("failed to write credentials to temp file: %v", err)
	}
	credsFile.Close()

	return storage.NewClient(ctx, option.WithCredentialsFile(credsFile.Name()))
}

// getCurrentMetadata retrieves the current WAV metadata from GCS
func getCurrentMetadata(ctx context.Context, bucket *storage.BucketHandle) (*WAVMetadata, error) {
	obj := bucket.Object(metadataFile)
	r, err := obj.NewReader(ctx)
	if err == storage.ErrObjectNotExist {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %v", err)
	}
	defer r.Close()

	var metadata WAVMetadata
	if err := json.NewDecoder(r).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %v", err)
	}

	return &metadata, nil
}

// updateMetadata saves the current WAV metadata to GCS
func updateMetadata(ctx context.Context, bucket *storage.BucketHandle, metadata *WAVMetadata) error {
	obj := bucket.Object(metadataFile)
	writer := obj.NewWriter(ctx)
	if err := json.NewEncoder(writer).Encode(metadata); err != nil {
		writer.Close()
		return fmt.Errorf("failed to encode metadata: %v", err)
	}
	return writer.Close()
}

// shouldCreateNewFile determines if we need to create a new WAV file
func shouldCreateNewFile(metadata *WAVMetadata) bool {
	if metadata == nil {
		return true
	}

	currentDuration := calculateDuration(metadata.CurrentSize)
	timeSinceLastWrite := time.Since(metadata.LastWriteTime)

	return currentDuration >= maxDuration || timeSinceLastWrite >= inactivityLimit
}

// createWAVHeader generates a WAV header for the given data length
func createWAVHeader(dataLength int) []byte {
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

// HandlePostAudio is the Cloud Function entrypoint
func HandlePostAudio(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()

	query := r.URL.Query()
	sampleRateParam := query.Get("sample_rate")
	uid := query.Get("uid")

	log.Printf("Received request from uid: %s", uid)
	log.Printf("Requested sample rate: %s", sampleRateParam)

	// Get bucket name from environment variable
	bucketName := os.Getenv("GCS_BUCKET_NAME")
	if bucketName == "" {
		log.Printf("GCS_BUCKET_NAME environment variable is not set")
		http.Error(w, "GCS_BUCKET_NAME environment variable is not set", http.StatusInternalServerError)
		return
	}

	// Create storage client
	client, err := getStorageClient(ctx)
	if err != nil {
		log.Printf("Failed to create storage client: %v", err)
		http.Error(w, fmt.Sprintf("Failed to create storage client: %v", err), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	bucket := client.Bucket(bucketName)

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Get current metadata
	metadata, err := getCurrentMetadata(ctx, bucket)
	if err != nil {
		log.Printf("Failed to get metadata: %v", err)
		http.Error(w, fmt.Sprintf("Failed to get metadata: %v", err), http.StatusInternalServerError)
		return
	}

	if shouldCreateNewFile(metadata) {
		// Create new WAV file
		currentTime := time.Now()
		filename := fmt.Sprintf("%02d_%02d_%04d_%02d_%02d_%02d.wav",
			currentTime.Day(),
			currentTime.Month(),
			currentTime.Year(),
			currentTime.Hour(),
			currentTime.Minute(),
			currentTime.Second())

		log.Printf("Creating new WAV file: %s", filename)

		// Create new file in GCS
		obj := bucket.Object(filename)
		writer := obj.NewWriter(ctx)
		writer.ContentType = "audio/wav"

		// Write header and body
		header := createWAVHeader(len(body))
		if _, err := writer.Write(header); err != nil {
			writer.Close()
			log.Printf("Failed to write header: %v", err)
			http.Error(w, "Failed to write header", http.StatusInternalServerError)
			return
		}
		if _, err := writer.Write(body); err != nil {
			writer.Close()
			log.Printf("Failed to write audio data: %v", err)
			http.Error(w, "Failed to write audio data", http.StatusInternalServerError)
			return
		}
		if err := writer.Close(); err != nil {
			log.Printf("Failed to close writer: %v", err)
			http.Error(w, "Failed to close writer", http.StatusInternalServerError)
			return
		}

		// Update metadata
		metadata = &WAVMetadata{
			Filename:      filename,
			LastWriteTime: currentTime,
			CurrentSize:   len(body),
		}
	} else {
		log.Printf("Appending to existing WAV file: %s", metadata.Filename)

		// Read existing file content
		oldObj := bucket.Object(metadata.Filename)
		reader, err := oldObj.NewReader(ctx)
		if err != nil {
			log.Printf("Failed to read existing file: %v", err)
			http.Error(w, "Failed to read existing file", http.StatusInternalServerError)
			return
		}

		existingContent, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			log.Printf("Failed to read existing content: %v", err)
			http.Error(w, "Failed to read existing content", http.StatusInternalServerError)
			return
		}

		// Create new content with updated header
		newSize := metadata.CurrentSize + len(body)
		header := createWAVHeader(newSize)
		
		// Combine header, existing audio data (excluding old header), and new audio data
		newContent := make([]byte, 0, len(header)+newSize)
		newContent = append(newContent, header...)
		newContent = append(newContent, existingContent[44:]...)
		newContent = append(newContent, body...)

		// Write new content
		writer := oldObj.NewWriter(ctx)
		writer.ContentType = "audio/wav"
		
		if _, err := writer.Write(newContent); err != nil {
			writer.Close()
			log.Printf("Failed to write new content: %v", err)
			http.Error(w, "Failed to write new content", http.StatusInternalServerError)
			return
		}
		if err := writer.Close(); err != nil {
			log.Printf("Failed to close writer: %v", err)
			http.Error(w, "Failed to close writer", http.StatusInternalServerError)
			return
		}

		// Update metadata
		metadata.CurrentSize = newSize
		metadata.LastWriteTime = time.Now()
	}

	// Save metadata
	if err := updateMetadata(ctx, bucket, metadata); err != nil {
		log.Printf("Failed to update metadata: %v", err)
		http.Error(w, fmt.Sprintf("Failed to update metadata: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully processed audio for file: %s", metadata.Filename)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf("Audio bytes processed for file %s", metadata.Filename)))
}