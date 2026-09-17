package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mhmdkzr/jev"
)

func main() {
	apiKey := os.Getenv("JEV_API_KEY")
	if apiKey == "" {
		log.Fatal("JEV_API_KEY not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := jev.NewClient(jev.WithAPIKey(apiKey))
	if err != nil {
		log.Fatal(err)
	}

	state := "Help! My payouts have been failing for 3 days."

	isUrgent := jev.Noul("is_urgent", "Does this convey urgency?").
		True("Explicitly time-sensitive").
		False("No urgency expressed")

	department := jev.Choice("department", "Which team should handle this?").
		Option("billing", "Payments, invoicing, refunds").
		Option("technical", "Bugs, outages, integrations").
		Option("sales", "Pricing, upgrades, new accounts")

	frustration := jev.Score("frustration", "How frustrated is the customer?").
		Level("Calm").
		Level("Frustrated").
		Level("Very angry")

	result, err := client.NewRequest().
		WithContext(ctx).
		State(state).
		Question(isUrgent).
		Question(department).
		Question(frustration).
		Send()
	if err != nil {
		log.Fatal(err)
	}

	urgencyAnswer, err := result.Get(isUrgent)
	if err != nil {
		log.Fatal(err)
	}

	departmentAnswer, err := result.Get(department)
	if err != nil {
		log.Fatal(err)
	}

	frustrationAnswer, err := result.Get(frustration)
	if err != nil {
		log.Fatal(err)
	}

	usage := result.Usage()
	input, _ := usage.Input()
	output, _ := usage.Output()

	fmt.Printf("urgent:      %.2f\n", urgencyAnswer.Yes)
	fmt.Printf("department:  %s (confidence %.2f)\n", departmentAnswer.Choice, departmentAnswer.Confidence)
	fmt.Printf("frustration: %.2f\n", frustrationAnswer.Score)
	fmt.Printf("usage:       %d input / %d output tokens\n", input, output)
}
