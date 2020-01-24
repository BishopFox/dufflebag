package main

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"sync"
)

var last_device_letter = 'a'

var mount_base = "/var/app/current/dufflebag"
var account_number = ""
var MAX_GOROUTINE_COUNT = 200

var pairsRE = regexp.MustCompile(`([A-Z]+)=(?:"(.*?)")`)

type BlockDevice struct {
	DeviceName     string
	Size           uint64
	Label          string
	UUID           string
	FilesystemType string
}

func listBlockDevices(parent_device string) ([]BlockDevice, string) {
	columns := []string{
		"NAME",   // name
		"SIZE",   // size
		"LABEL",  // filesystem label
		"UUID",   // filesystem UUID
		"FSTYPE", // filesystem type
		"TYPE",   // device type
	}

	//fmt.Printf("executing lsblk")
	output, err := exec.Command(
		"lsblk",
		"-b", // output size in bytes
		"-P", // output fields as key=value pairs
		"-o", strings.Join(columns, ","),
		parent_device,
	).Output()
	if err != nil {
		return nil, "cannot list block devices: lsblk failed"
	}

	blockDeviceMap := make(map[string]BlockDevice)
	s := bufio.NewScanner(bytes.NewReader(output))
	for s.Scan() {
		pairs := pairsRE.FindAllStringSubmatch(s.Text(), -1)
		var dev BlockDevice
		var deviceType string
		for _, pair := range pairs {
			switch pair[1] {
			case "NAME":
				dev.DeviceName = pair[2]
			case "SIZE":
				size, err := strconv.ParseUint(pair[2], 10, 64)
				if err != nil {
					fmt.Printf(
						"invalid size %q from lsblk: %v", pair[2], err,
					)
				} else {
					// the number of bytes in a MiB.
					dev.Size = size / 1024 * 1024
				}
			case "LABEL":
				dev.Label = pair[2]
			case "UUID":
				dev.UUID = pair[2]
			case "FSTYPE":
				dev.FilesystemType = pair[2]
			case "TYPE":
				deviceType = pair[2]
			default:
				fmt.Printf("unexpected field from lsblk: %q", pair[1])
			}
		}

		// Partitions may not be used, as there is no guarantee that the
		// partition will remain available (and we don't model hierarchy).
		if deviceType == "loop" {
			continue
		}

		blockDeviceMap[dev.DeviceName] = dev
	}
	if err := s.Err(); err != nil {
		return nil, "cannot parse lsblk output"
	}

	blockDevices := make([]BlockDevice, 0, len(blockDeviceMap))
	for _, dev := range blockDeviceMap {
		blockDevices = append(blockDevices, dev)
	}
	return blockDevices, ""
}

// TODO This may need to be made atomic. Possible race conditions here
func get_device_name() string {
	last_device_letter += 1
	if last_device_letter == 'z' {
		last_device_letter = 'b'
	}
	return string("/dev/xvd" + string(last_device_letter))
}

func get_instance_id() string {
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/instance-id")
	if err != nil {
		fmt.Printf("ERROR getting instance ID from metadata URL: %s\n", err)
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	return string(body)
}

func get_availability_zone() string {
	resp, err := http.Get("http://169.254.169.254/latest/meta-data/placement/availability-zone")
	if err != nil {
		fmt.Printf("ERROR getting AZ from metadata URL: %s\n", err)
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	return string(body)
}

func wait_for_device_to_appear(device_name string) bool {
	// Try for 2 minutes to wait for our device to be available
	for i := 0; i < 60; i++ {
		blockDevices, _ := listBlockDevices(device_name)
		if len(blockDevices) > 0 {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// Waits for the given snapshot to be ready
func wait_for_snapshot_ready(snapshot_id string, ec2_svc *ec2.EC2) (bool, string) {
	// Try for 2 minutes to wait for our snapshot to become available
	for i := 0; i < 60; i++ {
		// Get the status of our new snapshot that we just made
		desc_input := &ec2.DescribeSnapshotsInput{
			SnapshotIds: []*string{
				&snapshot_id,
			},
		}
		snapshot_result, _ := ec2_svc.DescribeSnapshots(desc_input)
		if len(snapshot_result.Snapshots) == 0 {
			fmt.Printf("ERROR: EBS list was empty. You probably didn't set up the IAM permissions correctly.\n")
		}
		// If we're not pending or completed, then something bad happened
		switch *snapshot_result.Snapshots[0].State {
		case "pending":
			time.Sleep(2 * time.Second)
		case "completed":
			return true, *snapshot_result.Snapshots[0].State
		default:
			fmt.Printf("ERROR: Got an error state from the copied snapshot: %s. Aborting.\n", *snapshot_result.Snapshots[0].State)
			return false, *snapshot_result.Snapshots[0].State
		}
	}
	return false, "timeout"
}

// Waits for a new volume to be completed
func wait_for_volume_detached(volume_id string, ec2_svc *ec2.EC2) (bool, string) {
	// Try for 2 minutes to wait for our snapshot to become available
	for i := 0; i < 60; i++ {
		// Get the status of our new snapshot that we just made
		desc_input := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{
				&volume_id,
			},
		}
		volume_result, _ := ec2_svc.DescribeVolumes(desc_input)
		if len(volume_result.Volumes) == 0 {
			fmt.Printf("ERROR: EBS list was empty. You probably didn't set up the IAM permissions correctly. Or it might have disappeared before we could check it.\n")
			return false, "error"
		}
		// If we're not creating or completed, then something bad happened
		switch *volume_result.Volumes[0].State {
		case "in-use":
			time.Sleep(2 * time.Second)
		case "completed":
			return true, *volume_result.Volumes[0].State
		case "available":
			return true, *volume_result.Volumes[0].State
		case "ready":
			return true, *volume_result.Volumes[0].State
		default:
			fmt.Printf("ERROR: Got an error state from the volume: %s. Aborting.\n", *volume_result.Volumes[0].State)
			return false, *volume_result.Volumes[0].State
		}
	}
	return false, "timeout"
}

// Waits for a new volume to be completed
func wait_for_volume_ready(volume_id string, ec2_svc *ec2.EC2) (bool, string) {
	// Try for 2 minutes to wait for our snapshot to become available
	for i := 0; i < 60; i++ {
		// Get the status of our new snapshot that we just made
		desc_input := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{
				&volume_id,
			},
		}
		volume_result, _ := ec2_svc.DescribeVolumes(desc_input)
		if len(volume_result.Volumes) == 0 {
			fmt.Printf("ERROR: EBS list was empty. You probably didn't set up the IAM permissions correctly.\n")
		}
		// If we're not creating or completed, then something bad happened
		switch *volume_result.Volumes[0].State {
		case "creating":
			time.Sleep(2 * time.Second)
		case "completed":
			return true, *volume_result.Volumes[0].State
		case "available":
			return true, *volume_result.Volumes[0].State
		default:
			fmt.Printf("ERROR: Got an error state from the volume: %s. Aborting.\n", *volume_result.Volumes[0].State)
			return false, *volume_result.Volumes[0].State
		}
	}
	return false, "timeout"
}

// Waits for the given volume to be attached
func wait_for_attaching_ready(volume_id string, ec2_svc *ec2.EC2) (bool, string) {
	// Try for 2 minutes to wait for our snapshot to become available
	for i := 0; i < 60; i++ {
		// Get the status of our new snapshot that we just made
		desc_input := &ec2.DescribeVolumesInput{
			VolumeIds: []*string{
				&volume_id,
			},
		}
		volume_result, _ := ec2_svc.DescribeVolumes(desc_input)
		if len(volume_result.Volumes) == 0 {
			fmt.Printf("ERROR: EBS list was empty. You probably didn't set up the IAM permissions correctly.\n")
		}
		// If we're not attaching or attached, then something bad happened
		switch *volume_result.Volumes[0].State {
		case "attaching":
			time.Sleep(2 * time.Second)
		case "attached":
			return true, "attached"
		case "in-use":
			return true, "in-use"
		case "available":
			return false, "available"
		default:
			fmt.Printf("ERROR: Got an error state from the attaching volume: %s. Aborting.\n", *volume_result.Volumes[0].State)
			return false, *volume_result.Volumes[0].State
		}
	}
	return false, "timeout"
}

func mount(device_name string, mount_point string) []string {
	var mountpoints []string

	// What are all the subdevices we need to try to mount?
	blockDevices, _ := listBlockDevices(device_name)
	for _, device := range blockDevices {
		// Make a directory for the device to mount to
		cmd := exec.Command("mkdir", "-p", mount_point+device.DeviceName)
		_, cmderr := cmd.Output()
		if cmderr != nil {
			fmt.Printf("mount point mkdir error: %s\n", cmderr.Error())
		}

		// Unmount on the directory, just in case something is straggling there
		exec.Command("sudo", "umount", "-f", mount_point).Output()

		// Actually do the mount
		cmd = exec.Command("sudo", "mount", "/dev/"+device.DeviceName, mount_point+device.DeviceName)
		var stderrBuff bytes.Buffer
		cmd.Stderr = &stderrBuff
		_, cmderr = cmd.Output()
		if cmderr == nil {
			mountpoints = append(mountpoints, mount_point+device.DeviceName)
		}

		// Change the file permissions to world readable
		exec.Command("sudo", "chmod", "-R", "o+r", mount_point+device.DeviceName).Output()
	}
	return mountpoints
}

func cleanup(mountpoints []string, volume_id string, snapshot_id string, ec2_svc *ec2.EC2) bool {
	return_val := true

	// Unmount the volume locally
	for _, mountpoint := range mountpoints {
		cmd := exec.Command("sudo", "umount", "-l", "-f", mountpoint)
		_, umounterr := cmd.Output()
		if umounterr != nil {
			fmt.Printf("umount error with volume %s on mount point %s: %s\n", volume_id, mountpoint, umounterr)
		}
	}

	// Detach the volume from AWS
	if volume_id != "" {
		force := true
		detach_input := &ec2.DetachVolumeInput{
			VolumeId: aws.String(volume_id),
			Force:    &force,
		}
		_, detach_err := ec2_svc.DetachVolume(detach_input)
		if detach_err != nil {
			fmt.Printf("Failed to detach volume: %s\n", volume_id)
			return_val = false
		}

		fmt.Printf("Waiting for volume %s to detach...\n", volume_id)
		vol_detached, _ := wait_for_volume_detached(volume_id, ec2_svc)
		if !vol_detached {
			fmt.Printf("ERROR Volume : %s never detached\n\tMaybe it's still mounted?\n", volume_id)
			return_val = false
		}

		// Delete the volume
		delete_vol_input := &ec2.DeleteVolumeInput{
			VolumeId: aws.String(volume_id),
		}
		_, err := ec2_svc.DeleteVolume(delete_vol_input)
		if err != nil {
			fmt.Printf("Failed to delete volume: %s with error: %s\n", volume_id, err.Error())
			return_val = false
		}
	}

	// Delete the snapshot we made
	if snapshot_id != "" {
		input := &ec2.DeleteSnapshotInput{
			SnapshotId: aws.String(snapshot_id),
		}
		_, error := ec2_svc.DeleteSnapshot(input)
		if error != nil {
			fmt.Printf("Failed to delete snapshot: %s\n", snapshot_id)
			return_val = false
		}
	}
	return return_val
}

func main() {
	fmt.Printf("\n\n")
	port := os.Getenv("PORT")
	if port == "" {
		port = "80"
	}

	// Concurrent blacklist mapsets used by inspector
	setupBlacklists()

	bucketname := ""
	// Get the dufflebag S3 bucket name
	sess, _ := session.NewSession(&aws.Config{
		Region: aws.String(aws_region)},
	)
	s3svc := s3.New(sess)
	result, err := s3svc.ListBuckets(nil)
	if err != nil {
		fmt.Printf("ERROR: Unable to list buckets. You probably didn't set up the IAM permissions correctly. %s\n", err)
		return
	}
	for _, bucket := range result.Buckets {
		if strings.HasPrefix(*bucket.Name, "dufflebag") {
			bucketname = *bucket.Name
			fmt.Printf("INFO: Uploading sensitive files to S3 bucket: %s\n", bucketname)
			break
		}
	}
	if bucketname == "" {
		fmt.Printf("ERROR: No dufflebag S3 bucket found. Make an S3 bucket with a name that starts with 'dufflebag' and try again. %s\n", err)
		return
	}

	// Posts to / indicate a new EBS volume to process
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if snapshot_id, err := ioutil.ReadAll(r.Body); err == nil {
			start_time := time.Now()
			fmt.Printf("Got new request from SQS with EBS snapshot ID: %s\n", string(snapshot_id))
			// First we have to "copy" the snapshot, which makes a volume out of it
			ec2_svc := ec2.New(session.New(), &aws.Config{
				Region: aws.String(aws_region)})
			copy_input := &ec2.CopySnapshotInput{
				Description:       aws.String("Created by Dufflebag"),
				DestinationRegion: aws.String(aws_region),
				SourceRegion:      aws.String(aws_region),
				SourceSnapshotId:  aws.String(string(snapshot_id)),
			}

			// Try copying the snapshot a few times
			// 	Amazon limits the number of concurrent snapshot copies that can be made at a time. (20 by default)
			// 	If we hit this limit, then the request will fail. But we really just need to try again
			copy_worked := false
			snapshot_id := ""
			for i := 0; i < 5; i++ {
				copy_result, copy_error := ec2_svc.CopySnapshot(copy_input)
				if copy_error == nil {
					copy_worked = true
					if copy_result.SnapshotId == nil {
						fmt.Printf("Error copying snapshot. ID came back null\n")
						cleanup([]string{}, "", snapshot_id, ec2_svc)
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte("500 - Failed to copy snapshot"))
						return
					}
					snapshot_id = *copy_result.SnapshotId
					break
				}	else {
					// Wait a little bit and then try again
					fmt.Printf("WARN: Copying snapshot failed. Trying again...\n")
					time.Sleep(5 * time.Second)
				}
			}

			if copy_worked == false {
				fmt.Printf("Copy Snapshot Error\n")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Copy Snapshot Error"))
				return
			}

			fmt.Printf("Copied the snapshot. Waiting for it to be ready...\n")
			// Wait for the snapshot to be ready
			snapshot_ready, snap_state := wait_for_snapshot_ready(snapshot_id, ec2_svc)
			if !snapshot_ready {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Waited for 10 minutes!"))
				return
			} else {
				fmt.Printf("Snapshot copy %s is %s\n", snapshot_id, snap_state)
			}

			// Get our availability zone
			availability_zone := get_availability_zone()

			// Make a new volume based on the snapshot
			isencrypted := true
			volume_input := &ec2.CreateVolumeInput{
				AvailabilityZone: aws.String(availability_zone),
				SnapshotId:       aws.String(snapshot_id),
				Encrypted:        &isencrypted,
			}
			volume_result, volume_error := ec2_svc.CreateVolume(volume_input)
			if volume_error != nil {
				fmt.Printf("Volume Create Error: %s\n", volume_error.Error())
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Failed to make volume"))
				return
			}

			// Wait for the volume to be ready
			fmt.Printf("Created a volume with ID: %s. Waiting for it to be ready...\n", *volume_result.VolumeId)
			volume_ready, vol_state := wait_for_volume_ready(*volume_result.VolumeId, ec2_svc)
			if !volume_ready {
				fmt.Printf("Volume is not ready. State: %s!\n", vol_state)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Waited for 10 minutes!"))
				return
			}

			// Get our instance ID
			instance_id := get_instance_id()
			device_name := ""

			// Attach the volume
			for i := 0; i < 24; i++ {
				device_name = get_device_name()
				attach_input := &ec2.AttachVolumeInput{
					Device:     aws.String(device_name),
					InstanceId: aws.String(instance_id),
					VolumeId:   aws.String(*volume_result.VolumeId),
				}
				attach_result, attach_err := ec2_svc.AttachVolume(attach_input)
				if attach_err == nil {
					fmt.Printf("Volume id used: %s!\n", *volume_result.VolumeId)
					fmt.Printf("Attach result: %s\n", attach_result)
					break
				} else {
					fmt.Printf("WARN: Attach to %s failed. Retrying on a new device...\n", device_name)
				}
			}

			mount_point_parent := mount_base + device_name

			// Wait for volume to attach
			fmt.Printf("Attached the volume to our instance, waiting for the volume to attach...\n")
			attach_ready, attach_state := wait_for_attaching_ready(*volume_result.VolumeId, ec2_svc)
			if !attach_ready {
				fmt.Printf("ERROR: Volume is not ready. State: %s!\n", attach_state)
				cleanup([]string{device_name}, *volume_result.VolumeId, snapshot_id, ec2_svc)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Waited for 10 minutes!"))
				return
			}

			// Wait for the device to appear locally
			fmt.Printf("Volume attached, waiting for device to appear locally...\n")
			if !wait_for_device_to_appear(device_name) {
				fmt.Printf("Error: Device %s never appeared.\n", device_name)
				cleanup([]string{device_name}, *volume_result.VolumeId, snapshot_id, ec2_svc)
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Waited for 10 minutes!"))
				return
			}

			fmt.Printf("Device %s appeared locally\n", device_name)

			// Mount the volume to the filesystem
			var mountpoints = mount(device_name, mount_point_parent)

			if len(mountpoints) == 0 {
				fmt.Printf("WARN: Mounted nothing for device %s, volume %s\n", device_name, *volume_result.VolumeId)
			}

			var waitgroup sync.WaitGroup
			limiter := make(chan bool, MAX_GOROUTINE_COUNT)
			for _, mountpoint := range mountpoints {
				// Pilfer the volume
				filepath.Walk(mountpoint, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return nil
					}

					// Ignore special files
					if !info.Mode().IsRegular() {
						return nil
					}

					// Ignore the file if it's bigger than 50MiB
					if info.Size() > 52428800 {
						return nil
					}

					// Scan the file for secrets
					waitgroup.Add(1)
					// Push a value into the limiter. If it's full, then we'll block here and wait for a spot to open
					limiter <- true
					go pilfer(limiter, &waitgroup, mountpoint, path, bucketname, *volume_result.VolumeId)
					return nil
				})
			}
			waitgroup.Wait()

			// Cleanup after ourselves
			if !cleanup(mountpoints, *volume_result.VolumeId, snapshot_id, ec2_svc) {
				fmt.Printf("Cleanup error\n")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - Failed to cleanup"))
				return
			}

			time_end := time.Now()
			fmt.Printf("Finished in time: %s\n", time_end.Sub(start_time))
		}
	})

	fmt.Printf("%s Started server. Listening on port %s\n", time.Now(), port)
	http.ListenAndServe(":"+port, nil)
}
