package main

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/zeebe-io/zeebe/clients/go/zbc"
)

/*
	what does this code need to do?
		- run for a set amount of time to simulate a long running process
		- callback to the zeebe broker with the result: SUCCESS/FAIL

	dependencies
		external
			- zeebe broker [x]
			- nomad cluster [x]
			- nomad job [x]
			- nomad job definition [x] - need to do some research on 'batch' jobs
			- nomad job docker image [x]

		internal
			- the amount of time to run for [x]
			- the URL of the zeebe broker [x]
			- an key to uniquely identify the zeebe job AKA zeebe 'job key' [x]
			- the zeebe 'payload' [x]
			- a flag to indicate if the job should succeed or fail [x]

	notes
		- zeebe payload is sent over the wire as JSON
		- zeebe job key is globally unique and continues to be valid after a timeout
*/

func main() {
	log.Printf("env: %q", os.Environ())

	durationStr := mustGetEnv("DURATION")
	zeebeBrokerURL := mustGetEnv("ZEEBE_BROKER_URL")
	zeebeFailJobFlag, err := strconv.ParseBool(mustGetEnv("ZEEBE_FAIL_JOB_FLAG"))
	if err != nil {
		log.Fatalf("error parsing zeebe fail job flag: %v", err)
	}

	zeebeJobKey, err := strconv.Atoi(mustGetEnv("ZEEBE_JOB_KEY"))
	if err != nil {
		log.Fatalf("error parsing zeebe job key: %v", err)
	}

	var zeebePayload map[string]interface{}
	if err := json.Unmarshal([]byte(mustGetEnv("ZEEBE_PAYLOAD")), &zeebePayload); err != nil {
		log.Fatalf("error parsing zeebe payload: %v", err)
	}

	client, err := zbc.NewZBClient(zeebeBrokerURL)
	if err != nil {
		log.Fatalf("error creating zeebe client: %#v", err)
	}

	duration, err := time.ParseDuration(durationStr)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("sleeping for", durationStr)
	time.Sleep(duration)

	if zeebeFailJobFlag {
		log.Println("setting error flag to true")
		zeebePayload["error"] = true
	}

	req, err := client.NewCompleteJobCommand().JobKey(int64(zeebeJobKey)).PayloadFromMap(zeebePayload)
	if err != nil {
		log.Fatalf("error completing zeebe job: %v", err)
	}

	log.Printf("sending payload: %q\n", zeebePayload)
	if _, err := req.Send(); err != nil {
		log.Fatalf("error sending zeebe 'Complete Job' command to broker: %v", err)
	}

	log.Println("Completed job", zeebeJobKey)
}

func mustGetEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("env var '%s' not set", k)
	}
	return v
}
