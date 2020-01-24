package main

import (
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sqs"
	"os/exec"
	"strings"
)

var mount_base = "/var/app/current/dufflebag"

// Goroutine thread to add one new SQS message with a snapshot ID
func populate(queue_url string, snapshot string) {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	sqs_svc := sqs.New(sess, &aws.Config{
		Region: aws.String(aws_region)})

	_, err := sqs_svc.SendMessage(&sqs.SendMessageInput{
		DelaySeconds: aws.Int64(0),
		MessageBody:  aws.String(snapshot),
		QueueUrl:     &queue_url,
	})

	if err != nil {
		fmt.Printf("Error %s\n", err)
		return
	}
}

func main() {
	fmt.Printf("Running populate...\n")

	cmd := exec.Command("mkdir", "-p", mount_base)
	_, cmderr := cmd.Output()
	if cmderr != nil {
		fmt.Printf("mkdir error: %s\n", cmderr.Error())
	}

	// Get a list of every public snapshot for the region
	ec2_svc := ec2.New(session.New(), &aws.Config{
		Region: aws.String(aws_region)})
	input := &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("encrypted"),
				Values: []*string{aws.String("false")},
			},
		},
	}
	snapshots_result, _ := ec2_svc.DescribeSnapshots(input)
	if len(snapshots_result.Snapshots) == 0 {
		fmt.Printf("ERROR: EBS list was empty. You probably didn't set up the IAM permissions correctly.\n")
	}

	sqssvc := sqs.New(session.New(), aws.NewConfig().WithRegion(aws_region))
	queue_url := ""
	params := &sqs.ListQueuesInput{}
	sqs_resp, err := sqssvc.ListQueues(params)
	if err != nil {
		fmt.Printf("SQS ERROR: %s\n", err.Error())
		return
	}
	for _, url := range sqs_resp.QueueUrls {
		if strings.Contains(*url, "AWSEBWorkerQueue") {
			queue_url = *url
		}
	}

	snapshots := snapshots_result.Snapshots
	//#####################################################################
	//####                    Safety Valve                             ####
	//#### Remove this line of code below to search all of your region ####
	//#####################################################################
	snapshots = snapshots_result.Snapshots[0:20]

	fmt.Printf("Using URL: %s\n", queue_url)
	fmt.Printf("Adding %d volumes to the queue\n", len(snapshots))

	// First, let's delete any existing messages in the queue. Could be left over from an old run
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	sqs_svc := sqs.New(sess, &aws.Config{
		Region: aws.String(aws_region)})
	params_purge := &sqs.PurgeQueueInput{
		QueueUrl: aws.String(queue_url),
	}
	_, err_purge := sqs_svc.PurgeQueue(params_purge)
	if err_purge != nil {
		fmt.Printf("ERROR: Couldn't purge the queue. %s\n", err_purge.Error())
	} else {
		fmt.Printf("Purged the queues. Waiting a bit for it to finish...\n")
	}

	// Check if the queue is empty
	for i := 0; i < 10; i++ {
		result, _ := sqs_svc.ReceiveMessage(&sqs.ReceiveMessageInput{
			WaitTimeSeconds: aws.Int64(1),
		})
		if len(result.Messages) == 0 {
			break
		}
	}

	fmt.Printf("Finished waiting for the purge. Pushing new items into the queue.\n")
	for _, snapshot := range snapshots {
		// Make doubly sure that the snapshot isn't encrypted
		if *snapshot.Encrypted == false {
			populate(queue_url, *snapshot.SnapshotId)
		} else {
			fmt.Printf("WARN: Skipping snapshot %s, it was encrypted. %t\n", *snapshot.SnapshotId, *snapshot.Encrypted == true)
		}
	}
	fmt.Printf("Finished inserting into the queue.\n")
}
