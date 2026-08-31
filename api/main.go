package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

type Config struct {
	Port               string
	AWSRegion          string
	S3BucketRaw        string
	S3BucketTranscoded string
	ECSCluster         string
	ECSTaskDefinition  string
	ECSContainerName   string
	ECSSubnets         string
	ECSSecurityGroups  string
	DatabaseURL        string
}

func loadConfig() Config {
	_ = godotenv.Load()

	return Config{
		Port:               os.Getenv("PORT"),
		AWSRegion:          os.Getenv("AWS_REGION"),
		S3BucketRaw:        os.Getenv("S3_BUCKET_RAW"),
		S3BucketTranscoded: os.Getenv("S3_BUCKET_TRANSCODED"),
		ECSCluster:         os.Getenv("ECS_CLUSTER"),
		ECSTaskDefinition:  os.Getenv("ECS_TASK_DEFINITION"),
		ECSContainerName:   os.Getenv("ECS_CONTAINER_NAME"),
		ECSSubnets:         os.Getenv("ECS_SUBNETS"),
		ECSSecurityGroups:  os.Getenv("ECS_SECURITY_GROUPS"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
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
	uploader := transfermanager.New(s3Client)

	ecsClient := ecs.NewFromConfig(awsCfg)

	dbConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to parse database config: %v", err)
	}

	dbConfig.ConnConfig.DefaultQueryExecMode =
		pgx.QueryExecModeSimpleProtocol

	dbPool, err := pgxpool.NewWithConfig(ctx, dbConfig)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer dbPool.Close()

	subnets := strings.Split(cfg.ECSSubnets, ",")
	securityGroups := strings.Split(cfg.ECSSecurityGroups, ",")

	r := chi.NewRouter()

	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(
			w,
			r.Body,
			2<<30,
		)

		err := r.ParseMultipartForm(32 << 20)
		if err != nil {
			http.Error(
				w,
				"failed to parse upload",
				http.StatusBadRequest,
			)
			return
		}

		videoFile, videoHeader, err := r.FormFile("video")
		if err != nil {
			http.Error(
				w,
				"video file is required",
				http.StatusBadRequest,
			)
			return
		}
		defer videoFile.Close()

		thumbnailFile, thumbnailHeader, err :=
			r.FormFile("thumbnail")

		if err != nil {
			http.Error(
				w,
				"thumbnail file is required",
				http.StatusBadRequest,
			)
			return
		}
		defer thumbnailFile.Close()

		videoContentType :=
			videoHeader.Header.Get("Content-Type")

		thumbnailContentType :=
			thumbnailHeader.Header.Get("Content-Type")

		if !strings.HasPrefix(
			videoContentType,
			"video/",
		) {
			http.Error(
				w,
				"uploaded video file is not a valid video",
				http.StatusBadRequest,
			)
			return
		}

		if !strings.HasPrefix(
			thumbnailContentType,
			"image/",
		) {
			http.Error(
				w,
				"uploaded thumbnail is not a valid image",
				http.StatusBadRequest,
			)
			return
		}

		videoID := uuid.New().String()

		videoKey := fmt.Sprintf(
			"raw/%s/original%s",
			videoID,
			filepath.Ext(videoHeader.Filename),
		)

		thumbKey := fmt.Sprintf(
			"thumbnails/%s/thumbnail%s",
			videoID,
			filepath.Ext(thumbnailHeader.Filename),
		)

		_, err = uploader.UploadObject(
			r.Context(),
			&transfermanager.UploadObjectInput{
				Bucket: aws.String(
					cfg.S3BucketRaw,
				),
				Key: aws.String(
					videoKey,
				),
				Body: videoFile,
				ContentType: aws.String(
					videoContentType,
				),
			},
		)

		if err != nil {
			http.Error(
				w,
				"failed to upload video to S3",
				http.StatusInternalServerError,
			)
			return
		}

		_, err = uploader.UploadObject(
			r.Context(),
			&transfermanager.UploadObjectInput{
				Bucket: aws.String(
					cfg.S3BucketRaw,
				),
				Key: aws.String(
					thumbKey,
				),
				Body: thumbnailFile,
				ContentType: aws.String(
					thumbnailContentType,
				),
			},
		)

		if err != nil {
			http.Error(
				w,
				"failed to upload thumbnail to S3",
				http.StatusInternalServerError,
			)
			return
		}

		thumbnailURL := fmt.Sprintf(
			"https://%s.s3.%s.amazonaws.com/%s",
			cfg.S3BucketRaw,
			cfg.AWSRegion,
			thumbKey,
		)

		_, err = dbPool.Exec(
			r.Context(),
			`
			INSERT INTO videos (
				id,
				status,
				raw_video_key,
				thumbnail_url
			)
			VALUES ($1, $2, $3, $4)
			`,
			videoID,
			"processing",
			videoKey,
			thumbnailURL,
		)

		if err != nil {
			http.Error(
				w,
				"failed to save video metadata",
				http.StatusInternalServerError,
			)
			return
		}

		log.Printf(
			"launching ECS task videoID=%s videoKey=%s",
			videoID,
			videoKey,
		)

		runResult, err := ecsClient.RunTask(
			r.Context(),
			&ecs.RunTaskInput{
				Cluster: aws.String(
					cfg.ECSCluster,
				),

				TaskDefinition: aws.String(
					cfg.ECSTaskDefinition,
				),

				LaunchType:
					types.LaunchTypeFargate,

				NetworkConfiguration:
					&types.NetworkConfiguration{
						AwsvpcConfiguration:
							&types.AwsVpcConfiguration{
								Subnets: subnets,

								SecurityGroups:
									securityGroups,

								AssignPublicIp:
									types.AssignPublicIpEnabled,
							},
					},

				Overrides: &types.TaskOverride{
					ContainerOverrides:
						[]types.ContainerOverride{
							{
								Name: aws.String(
									cfg.ECSContainerName,
								),

								Environment:
									[]types.KeyValuePair{
										{
											Name: aws.String(
												"VIDEO_ID",
											),
											Value: aws.String(
												videoID,
											),
										},
										{
											Name: aws.String(
												"S3_BUCKET_RAW",
											),
											Value: aws.String(
												cfg.S3BucketRaw,
											),
										},
										{
											Name: aws.String(
												"S3_BUCKET_TRANSCODED",
											),
											Value: aws.String(
												cfg.S3BucketTranscoded,
											),
										},
										{
											Name: aws.String(
												"VIDEO_KEY",
											),
											Value: aws.String(
												videoKey,
											),
										},
										{
											Name: aws.String(
												"DATABASE_URL",
											),
											Value: aws.String(
												cfg.DatabaseURL,
											),
										},
										{
											Name: aws.String(
												"AWS_REGION",
											),
											Value: aws.String(
												cfg.AWSRegion,
											),
										},
									},
							},
						},
				},
			},
		)

		if err != nil {
			http.Error(
				w,
				"failed to start transcoding task",
				http.StatusInternalServerError,
			)
			return
		}

		if len(runResult.Failures) > 0 {
			log.Printf(
				"ECS RunTask failures: %+v",
				runResult.Failures,
			)

			http.Error(
				w,
				"ECS could not start transcoding task",
				http.StatusInternalServerError,
			)
			return
		}

		log.Printf(
			"ECS worker launched for video %s",
			videoID,
		)

		w.Header().Set(
			"Content-Type",
			"application/json",
		)

		w.WriteHeader(http.StatusAccepted)

		fmt.Fprintf(
			w,
			`{"video_id":"%s","status":"processing"}`,
			videoID,
		)
	})

	port := cfg.Port

	if port == "" {
		port = "8080"
	}

	log.Printf(
		"server starting on port %s",
		port,
	)

	log.Fatal(
		http.ListenAndServe(
			":"+port,
			r,
		),
	)
}