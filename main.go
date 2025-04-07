package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corePb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	statusPb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/sashabaranov/go-openai"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
)

type server struct{}
type healthServer struct{}

var (
	apiKey      = os.Getenv("GUARDIAN_API_KEY")
	baseURL     = os.Getenv("GUARDIAN_URL")
	fullBaseURL = baseURL + "/openai/v1"
	modelName   = "granite-guardian"
	riskyToken  = "Yes"
	client      openai.Client
)

func init() {
	if apiKey == "" {
		log.Fatal("GUARDIAN_API_KEY env var is not set")
	}
	if baseURL == "" {
		log.Fatal("GUARDIAN_URL env var is not set")
	}

	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = fullBaseURL
	client = *openai.NewClientWithConfig(cfg)
}

func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	log.Printf("[HealthCheck] Received health check request: %+v", in)
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	log.Printf("[HealthWatch] Received watch request: %+v", in)
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

func (s *server) Process(srv extProcPb.ExternalProcessor_ProcessServer) error {
	log.Println("[Process] Starting processing loop")
	for {
		req, err := srv.Recv()
		if err == io.EOF {
			log.Println("[Process] Received EOF, terminating processing loop")
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				log.Println("[Process] Stream cancelled, finishing up")
				return nil
			}
			log.Printf("[Process] Error receiving request: %v", err)
			return err
		}

		log.Printf("[Process] Received request: %+v", req)

		var resp *extProcPb.ProcessingResponse

		switch r := req.Request.(type) {
		case *extProcPb.ProcessingRequest_RequestHeaders:
			log.Println("[Process] Processing RequestHeaders")
			// pass through headers untouched
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extProcPb.HeadersResponse{},
				},
			}
			log.Println("[Process] RequestHeaders processed, passing through response unchanged")

		case *extProcPb.ProcessingRequest_RequestBody:
			log.Println("[Process] Processing RequestBody")

			bodyStr := string(r.RequestBody.Body)
			log.Printf("[Process] Request body: %s", bodyStr)

			var bodyMap map[string]interface{}
			if err := json.Unmarshal([]byte(bodyStr), &bodyMap); err != nil {
				log.Printf("[Process] Failed to parse request body: %v", err)
				return status.Errorf(codes.InvalidArgument, "invalid request body: %v", err)
			}

			prompt, _ := bodyMap["prompt"].(string)
			log.Printf("[Process] Extracted prompt: %s", prompt)

			if os.Getenv("DISABLE_PROMPT_RISK_CHECK") == "yes" {
				log.Println("[Process] Prompt risk check disabled via env var, allowing request")
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_RequestBody{
						RequestBody: &extProcPb.BodyResponse{},
					},
				}
			} else {
				if checkRisk(prompt) {
					log.Println("[Process] Risky prompt detected, returning 403")

					resp = &extProcPb.ProcessingResponse{
						Response: &extProcPb.ProcessingResponse_ImmediateResponse{
							ImmediateResponse: &extProcPb.ImmediateResponse{
								Status: &statusPb.HttpStatus{
									Code: statusPb.StatusCode_Forbidden,
								},
								Body: []byte(`{"error":"Prompt blocked by content policy"}`),
								Headers: &extProcPb.HeaderMutation{
									SetHeaders: []*corePb.HeaderValueOption{
										{
											Header: &corePb.HeaderValue{
												Key:   "Content-Type",
												Value: "application/json",
											},
										},
									},
								},
							},
						},
					}
				} else {
					log.Println("[Process] Prompt safe, allowing request")
					resp = &extProcPb.ProcessingResponse{
						Response: &extProcPb.ProcessingResponse_RequestBody{
							RequestBody: &extProcPb.BodyResponse{},
						},
					}
				}
			}

			if err := srv.Send(resp); err != nil {
				log.Printf("[Process] Error sending response: %v", err)
				return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
			}

		case *extProcPb.ProcessingRequest_ResponseHeaders:
			log.Println("[Process] Processing ResponseHeaders, instructing Envoy to buffer response body")
			resp = &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extProcPb.HeadersResponse{},
				},
				ModeOverride: &filterPb.ProcessingMode{
					ResponseHeaderMode: filterPb.ProcessingMode_SKIP,
					ResponseBodyMode:   filterPb.ProcessingMode_BUFFERED,
				},
			}
			log.Println("[Process] ResponseHeaders processed, buffering response body")
			if err := srv.Send(resp); err != nil {
				log.Printf("[Process] Error sending response headers: %v", err)
				return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
			}

		case *extProcPb.ProcessingRequest_ResponseBody:
			log.Println("[Process] Processing ResponseBody")
			rb := r.ResponseBody
			log.Printf("[Process] ResponseBody received, EndOfStream: %v", rb.EndOfStream)

			if !rb.EndOfStream {
				log.Println("[Process] ResponseBody not complete, continuing to buffer")
				break
			}

			bodyStr := string(rb.Body)
			log.Printf("[Process] Full response body: %s", bodyStr)

			var respData map[string]interface{}
			if err := json.Unmarshal(rb.Body, &respData); err != nil {
				log.Printf("[Process] Failed to parse response body: %v", err)
				return status.Errorf(codes.InvalidArgument, "invalid response body: %v", err)
			}

			var generated string
			choices, ok := respData["choices"].([]interface{})
			if ok && len(choices) > 0 {
				first, _ := choices[0].(map[string]interface{})
				generated, _ = first["text"].(string)
			}
			log.Printf("[Process] Extracted response text: %s", generated)

			if os.Getenv("DISABLE_RESPONSE_RISK_CHECK") == "yes" {
				log.Println("[Process] Response risk check disabled via env var, allowing response")
				resp = &extProcPb.ProcessingResponse{
					Response: &extProcPb.ProcessingResponse_ResponseBody{
						ResponseBody: &extProcPb.BodyResponse{},
					},
				}
			} else {
				if checkRisk(generated) {
					log.Println("[Process] Risky LLM output detected, blocking response")
					resp = &extProcPb.ProcessingResponse{
						Response: &extProcPb.ProcessingResponse_ImmediateResponse{
							ImmediateResponse: &extProcPb.ImmediateResponse{
								Status: &statusPb.HttpStatus{
									Code: statusPb.StatusCode_Forbidden,
								},
								Body: []byte(`{"error":"LLM output blocked by safety filter"}`),
								Headers: &extProcPb.HeaderMutation{
									SetHeaders: []*corePb.HeaderValueOption{
										{
											Header: &corePb.HeaderValue{
												Key:   "Content-Type",
												Value: "application/json",
											},
										},
									},
								},
							},
						},
					}
				} else {
					log.Println("[Process] LLM output safe, allowing response")
					resp = &extProcPb.ProcessingResponse{
						Response: &extProcPb.ProcessingResponse_ResponseBody{
							ResponseBody: &extProcPb.BodyResponse{},
						},
					}
				}
			}

			if err := srv.Send(resp); err != nil {
				log.Printf("[Process] Error sending response: %v", err)
				return status.Errorf(codes.Unknown, "cannot send stream response: %v", err)
			}

		default:
			log.Printf("[Process] Received unrecognized request type: %+v", r)
			resp = &extProcPb.ProcessingResponse{}
		}
	}
}

func checkRisk(userQuery string) bool {
	log.Printf("👮‍♀️ [Guardian] Checking risk on: '%s'\n", userQuery)
	log.Printf("→ Sending to: %s/chat/completions with model '%s'\n", fullBaseURL, modelName)

	resp, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model: modelName,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: userQuery,
			},
		},
		Temperature: 0.01,
		MaxTokens:   50,
	})
	if err != nil {
		log.Fatalf("Risk model call failed: %v", err)
	}

	result := strings.TrimSpace(resp.Choices[0].Message.Content)
	log.Printf("🛡️ Risk Model Response: %s\n", result)

	return strings.EqualFold(result, riskyToken)
}

func main() {
	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		log.Fatalf("[Main] Failed to listen: %v", err)
	}
	s := grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s, &server{})
	healthPb.RegisterHealthServer(s, &healthServer{})
	log.Println("[Main] Starting gRPC server on port :50051")

	gracefulStop := make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-gracefulStop
		log.Println("[Main] Received shutdown signal, exiting after 1 second")
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	if err := s.Serve(lis); err != nil {
		log.Fatalf("[Main] Failed to serve: %v", err)
	}
}
