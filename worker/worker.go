package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
)

type Config struct {
	VideoID            string
	S3BucketRaw        string
	S3BucketTranscoded string
	VideoKey           string
	DatabaseURL        string
	AWSRegion          string
}

type qualityPreset struct {
	Name   string
	Height string
}

var qualityPresets = []qualityPreset{
	{Name: "1080p", Height: "1080"},
	{Name: "720p", Height: "720"},
	{Name: "480p", Height: "480"},
	{Name: "360p", Height: "360"},
}

func loadConfig() Config {
	_ = godotenv.Load()

	return Config{
		VideoID:            os.Getenv("VIDEO_ID"),
		S3BucketRaw:        os.Getenv("S3_BUCKET_RAW"),
		S3BucketTranscoded: os.Getenv("S3_BUCKET_TRANSCODED"),
		VideoKey:           os.Getenv("VIDEO_KEY"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		AWSRegion:          os.Getenv("AWS_REGION"),
	}
}

func main() {
	cfg := loadConfig()

	ctx := context.Background()

	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(cfg.AWSRegion),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	s3Client := s3.NewFromConfig(awsCfg)

	tempDir, err := os.MkdirTemp("", "transcode-*")
	if err != nil {
		log.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	inputPath := filepath.Join(
		tempDir,
		"input"+filepath.Ext(cfg.VideoKey),
	)

	inputFile, err := os.Create(inputPath)
	if err != nil {
		log.Fatalf("failed to create local input file: %v", err)
	}

	result, err := s3Client.GetObject(
		ctx,
		&s3.GetObjectInput{
			Bucket: aws.String(cfg.S3BucketRaw),
			Key:    aws.String(cfg.VideoKey),
		},
	)
	if err != nil {
		inputFile.Close()
		log.Fatalf("failed to download video from S3: %v", err)
	}
	defer result.Body.Close()

	_, err = io.Copy(inputFile, result.Body)
	if err != nil {
		inputFile.Close()
		log.Fatalf("failed to save downloaded video: %v", err)
	}

	if err := inputFile.Close(); err != nil {
		log.Fatalf("failed to close input file: %v", err)
	}

	log.Printf("downloaded video to %s", inputPath)

	outputPaths := make(map[string]string)

	for _, preset := range qualityPresets {
		outputPath := filepath.Join(
			tempDir,
			preset.Name+".mp4",
		)

		cmd := exec.CommandContext(
			ctx,
			"ffmpeg",

			"-i", inputPath,

			"-vf", fmt.Sprintf(
				"scale=-2:%s",
				preset.Height,
			),

			"-c:v", "libx264",

			"-crf", "23",

			"-preset", "fast",

			"-c:a", "aac",

			"-b:a", "128k",

			"-y",

			outputPath,
		)

		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Fatalf(
				"ffmpeg failed for %s: %v\n%s",
				preset.Name,
				err,
				string(output),
			)
		}

		outputPaths[preset.Name] = outputPath

		log.Printf(
			"transcoded %s -> %s",
			preset.Name,
			outputPath,
		)
	}
}

