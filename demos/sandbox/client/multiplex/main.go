// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type ProcessRequest struct {
	Command []string          `json:"command"`
	EnvVars map[string]string `json:"envvars,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

type ProcessResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error,omitempty"`
}

func dialAteAPI(endpoint string) (ateapipb.ControlClient, *grpc.ClientConn, error) {
	creds := credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, err
	}
	return ateapipb.NewControlClient(conn), conn, nil
}

func runCommand(ctx context.Context, atenetAddr, atespace, actorID, command string) (*ProcessResponse, error) {
	url := fmt.Sprintf("http://%s/process", atenetAddr)
	reqBody := ProcessRequest{
		Command: []string{"sh", "-c", command},
	}
	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Host = resources.ActorDNSName(atespace, actorID)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("attempt %d failed to send request: %w", attempt, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("attempt %d got status code %d: %s", attempt, resp.StatusCode, string(body))
			// Retry on 503 or other server-side transient issues
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			return nil, lastErr
		}

		var processResp ProcessResponse
		if err := json.NewDecoder(resp.Body).Decode(&processResp); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("failed to decode response: %w", err)
		}
		resp.Body.Close()
		return &processResp, nil
	}

	return nil, fmt.Errorf("after 10 attempts: %w", lastErr)
}

type DemoStats struct {
	TotalResumes   int64   `json:"total_resumes"`
	WriteSuccesses int64   `json:"write_successes"`
	WriteFailures  int64   `json:"write_failures"`
	ReadSuccesses  int64   `json:"read_successes"`
	ReadFailures   int64   `json:"read_failures"`

	CreateLatencyP90Ms  float64 `json:"create_latency_p90_ms"`
	CreateThroughput    float64 `json:"create_throughput"`
	ResumeLatencyP90Ms  float64 `json:"resume_latency_p90_ms"`
	ResumeThroughput    float64 `json:"resume_throughput"`
	SuspendLatencyP90Ms float64 `json:"suspend_latency_p90_ms"`
	SuspendThroughput   float64 `json:"suspend_throughput"`
}

var metrics struct {
	mu sync.Mutex

	createStartTime time.Time
	createLatencies []int64

	resumeStartTime time.Time
	resumeLatencies []int64

	suspendStartTime time.Time
	suspendLatencies []int64
}

func recordCreate(dur time.Duration) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.createLatencies = append(metrics.createLatencies, dur.Milliseconds())
}

func recordResume(dur time.Duration) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.resumeLatencies = append(metrics.resumeLatencies, dur.Milliseconds())
}

func recordSuspend(dur time.Duration) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.suspendLatencies = append(metrics.suspendLatencies, dur.Milliseconds())
}

func getP90(latencies []int64) float64 {
	if len(latencies) == 0 {
		return 0.0
	}
	tmp := make([]int64, len(latencies))
	copy(tmp, latencies)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	idx := int(float64(len(tmp)) * 0.9)
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return float64(tmp[idx])
}

func main() {
	ateapiAddr := flag.String("ateapi", "localhost:8080", "Address of the ateapi gRPC server")
	atenetAddr := flag.String("atenet", "localhost:8000", "Address of the atenet HTTP router")
	atespace := flag.String("atespace", "sandbox-multiplex", "Atespace for the demo actors")
	numActors := flag.Int("actors", 10, "Number of actors to run")
	concurrency := flag.Int("concurrency", 2, "Number of concurrent actors running at a time")
	flag.Parse()

	ctx := context.Background()

	// 1. Connect to ateapi
	log.Printf("Connecting to ateapi at %s...", *ateapiAddr)
	cli, conn, err := dialAteAPI(*ateapiAddr)
	if err != nil {
		log.Fatalf("Failed to dial ateapi: %v", err)
	}
	defer conn.Close()

	var totalResumes int64
	var writeSuccesses, writeFailures int64
	var readSuccesses, readFailures int64

	updateStats := func() {
		metrics.mu.Lock()
		defer metrics.mu.Unlock()

		var createP90, createThroughput float64
		createCount := len(metrics.createLatencies)
		if createCount > 0 {
			createP90 = getP90(metrics.createLatencies)
			elapsed := time.Since(metrics.createStartTime).Seconds()
			if elapsed > 0 {
				createThroughput = float64(createCount) / elapsed
			}
		}

		var resumeP90, resumeThroughput float64
		resumeCount := len(metrics.resumeLatencies)
		if resumeCount > 0 {
			resumeP90 = getP90(metrics.resumeLatencies)
			elapsed := time.Since(metrics.resumeStartTime).Seconds()
			if elapsed > 0 {
				resumeThroughput = float64(resumeCount) / elapsed
			}
		}

		var suspendP90, suspendThroughput float64
		suspendCount := len(metrics.suspendLatencies)
		if suspendCount > 0 {
			suspendP90 = getP90(metrics.suspendLatencies)
			elapsed := time.Since(metrics.suspendStartTime).Seconds()
			if elapsed > 0 {
				suspendThroughput = float64(suspendCount) / elapsed
			}
		}

		stats := DemoStats{
			TotalResumes:        atomic.LoadInt64(&totalResumes),
			WriteSuccesses:      atomic.LoadInt64(&writeSuccesses),
			WriteFailures:       atomic.LoadInt64(&writeFailures),
			ReadSuccesses:       atomic.LoadInt64(&readSuccesses),
			ReadFailures:        atomic.LoadInt64(&readFailures),
			CreateLatencyP90Ms:  createP90,
			CreateThroughput:    createThroughput,
			ResumeLatencyP90Ms:  resumeP90,
			ResumeThroughput:    resumeThroughput,
			SuspendLatencyP90Ms: suspendP90,
			SuspendThroughput:   suspendThroughput,
		}

		data, err := json.Marshal(stats)
		if err == nil {
			_ = os.MkdirAll("./demos/sandbox/bin", 0755)
			_ = os.WriteFile("./demos/sandbox/bin/stats.json", data, 0644)
		}
	}
	updateStats()

	// 2. Ensure atespace exists
	log.Printf("Ensuring atespace %q exists...", *atespace)
	_, err = cli.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{Name: *atespace})
	if err != nil {
		log.Printf("CreateAtespace warning: %v", err)
	}



	// 4. Print initial worker list
	workersResp, err := cli.ListWorkers(ctx, &ateapipb.ListWorkersRequest{})
	if err != nil {
		log.Fatalf("Failed to list workers: %v", err)
	}
	log.Println("--- Initial Worker Pool State ---")
	for _, w := range workersResp.GetWorkers() {
		log.Printf("Worker Pod: %s (IP: %s, Node: %s)", w.GetWorkerPod(), w.GetIp(), w.GetNodeName())
	}
	log.Println("--------------------------------")

	// 5. Wait for key press before starting
	fmt.Print("\033[H\033[2J") // Clear screen
	fmt.Println(">>> Press [Enter] to start the multiplexing demonstration <<<")
	fmt.Scanln()

	// 6. Run the multiplexing test:
	// We write a unique state file into each actor, limiting concurrency using
	// a semaphore, with randomized delays to make it look natural.
	log.Println("Writing state (writing a file with a unique string) to each actor asynchronously...")
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup

	metrics.createStartTime = time.Now()
	metrics.resumeStartTime = time.Now()
	metrics.suspendStartTime = time.Now()

	for i := 1; i <= *numActors; i++ {
		wg.Add(1)
		go func(actorIdx int) {
			defer wg.Done()

			actorName := fmt.Sprintf("sandbox-%d", actorIdx)

			// 1. Create the actor in parallel (no concurrency limit)
			tCreate := time.Now()
			_, err := cli.CreateActor(ctx, &ateapipb.CreateActorRequest{
				ActorRef:               &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
				ActorTemplateNamespace: "ate-demo-sandbox",
				ActorTemplateName:      "sandbox-template",
			})
			durCreate := time.Since(tCreate)
			if err != nil {
				log.Printf("[Actor %d/%d] Create failed: %v", actorIdx, *numActors, err)
			} else {
				recordCreate(durCreate)
				updateStats()
			}

			// Acquire slot
			sem <- struct{}{}
			defer func() { <-sem }()

			// 2. Natural delay before starting resume/command
			startDelay := time.Duration(100+rand.Intn(400)) * time.Millisecond
			time.Sleep(startDelay)

			stateVal := fmt.Sprintf("state-value-for-actor-%d", actorIdx)
			cmd := fmt.Sprintf("echo -n '%s' > /tmp/state.txt", stateVal)

			log.Printf("[Actor %d/%d] Requesting write command (delay %v): %s", actorIdx, *numActors, startDelay, cmd)
			t0 := time.Now()
			resp, err := runCommand(ctx, *atenetAddr, *atespace, actorName, cmd)
			if err != nil {
				log.Printf("[Actor %d/%d] Write failed: %v", actorIdx, *numActors, err)
				atomic.AddInt64(&writeFailures, 1)
				updateStats()
				return
			}
			duration := time.Since(t0)

			if resp.Error != "" {
				log.Printf("[Actor %d/%d] Write command returned error: %s", actorIdx, *numActors, resp.Error)
				atomic.AddInt64(&writeFailures, 1)
				updateStats()
				return
			}
			log.Printf("[Actor %d/%d] Write successful in %v.", actorIdx, *numActors, duration)
			atomic.AddInt64(&writeSuccesses, 1)
			atomic.AddInt64(&totalResumes, 1)
			recordResume(duration)
			updateStats()

			// 2. Natural hold time while running before suspending
			holdDelay := time.Duration(800+rand.Intn(1200)) * time.Millisecond
			log.Printf("[Actor %d/%d] Holding worker for %v...", actorIdx, *numActors, holdDelay)
			time.Sleep(holdDelay)

			// 3. Suspend actor
			tSuspend := time.Now()
			_, err = cli.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
				ActorRef: &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
			})
			durSuspend := time.Since(tSuspend)
			if err != nil {
				log.Printf("[Actor %d/%d] Failed to suspend actor: %v", actorIdx, *numActors, err)
			} else {
				log.Printf("[Actor %d/%d] Actor suspended in %v.", actorIdx, *numActors, durSuspend)
				recordSuspend(durSuspend)
				updateStats()
			}
		}(i)
	}
	wg.Wait()

	// 7. Now read the state back from all actors with the concurrency limit and random delays.
	log.Println("\nReading state back from each actor asynchronously to verify seamless state persistence...")
	for i := 1; i <= *numActors; i++ {
		wg.Add(1)
		go func(actorIdx int) {
			defer wg.Done()

			// Acquire slot
			sem <- struct{}{}
			defer func() { <-sem }()

			// 1. Natural delay before starting resume/command
			startDelay := time.Duration(100+rand.Intn(400)) * time.Millisecond
			time.Sleep(startDelay)

			actorName := fmt.Sprintf("sandbox-%d", actorIdx)
			expectedState := fmt.Sprintf("state-value-for-actor-%d", actorIdx)
			cmd := "cat /tmp/state.txt"

			log.Printf("[Actor %d/%d] Requesting read command (delay %v): %s", actorIdx, *numActors, startDelay, cmd)
			t0 := time.Now()
			resp, err := runCommand(ctx, *atenetAddr, *atespace, actorName, cmd)
			if err != nil {
				log.Printf("[Actor %d/%d] Read failed: %v", actorIdx, *numActors, err)
				atomic.AddInt64(&readFailures, 1)
				updateStats()
				return
			}
			duration := time.Since(t0)

			if resp.Error != "" {
				log.Printf("[Actor %d/%d] Read command returned error: %s", actorIdx, *numActors, resp.Error)
				atomic.AddInt64(&readFailures, 1)
				updateStats()
				return
			}

			stdout := strings.TrimSpace(resp.Stdout)
			if stdout != expectedState {
				log.Printf("[Actor %d/%d] State mismatch! Expected %q, got %q", actorIdx, *numActors, expectedState, stdout)
				atomic.AddInt64(&readFailures, 1)
				updateStats()
				return
			}
			log.Printf("[Actor %d/%d] Read successful: %q (took %v).", actorIdx, *numActors, stdout, duration)
			atomic.AddInt64(&readSuccesses, 1)
			atomic.AddInt64(&totalResumes, 1)
			recordResume(duration)
			updateStats()

			// 2. Natural hold time while running before suspending
			holdDelay := time.Duration(800+rand.Intn(1200)) * time.Millisecond
			log.Printf("[Actor %d/%d] Holding worker for %v...", actorIdx, *numActors, holdDelay)
			time.Sleep(holdDelay)

			// 3. Suspend actor
			tSuspend := time.Now()
			_, err = cli.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
				ActorRef: &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
			})
			durSuspend := time.Since(tSuspend)
			if err != nil {
				log.Printf("[Actor %d/%d] Failed to suspend actor: %v", actorIdx, *numActors, err)
			} else {
				log.Printf("[Actor %d/%d] Actor suspended in %v.", actorIdx, *numActors, durSuspend)
				recordSuspend(durSuspend)
				updateStats()
			}
		}(i)
	}
	wg.Wait()

	log.Println("\n========================================")
	log.Println("Multiplexing Test Execution Summary:")
	log.Printf("Write Phase: %d succeeded, %d failed", atomic.LoadInt64(&writeSuccesses), atomic.LoadInt64(&writeFailures))
	log.Printf("Read Phase:  %d succeeded, %d failed", atomic.LoadInt64(&readSuccesses), atomic.LoadInt64(&readFailures))
	log.Println("========================================")

	// 8. Cleanup Prompt
	fmt.Print("\nDo you want to delete the created actors? [Y/n]: ")
	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))

	if response == "" || response == "y" || response == "yes" {
		log.Println("\nCleaning up created actors...")
		for i := 1; i <= *numActors; i++ {
			actorName := fmt.Sprintf("sandbox-%d", i)
			log.Printf("Deleting actor %s...", actorName)
			_, err := cli.DeleteActor(ctx, &ateapipb.DeleteActorRequest{
				ActorRef: &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
			})
			if err != nil {
				log.Printf("Delete failed for %s (%v), attempting to suspend and retry...", actorName, err)
				_, _ = cli.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
					ActorRef: &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
				})
				time.Sleep(500 * time.Millisecond)
				_, err = cli.DeleteActor(ctx, &ateapipb.DeleteActorRequest{
					ActorRef: &ateapipb.ActorRef{Atespace: *atespace, Name: actorName},
				})
				if err != nil {
					log.Printf("Failed to delete actor %s after retry: %v", actorName, err)
				}
			}
		}
	} else {
		log.Println("\nSkipping actor deletion.")
	}

	if atomic.LoadInt64(&writeFailures) > 0 || atomic.LoadInt64(&readFailures) > 0 {
		log.Fatalf("Multiplexing test finished with failures.")
	} else {
		log.Println("Multiplexing test completed successfully!")
	}
}
