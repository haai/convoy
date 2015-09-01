package ebs

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/service/ec2"
	"io/ioutil"
	"strconv"
	"strings"
	"time"
)

const (
	GB = 1073741824
)

var (
	log = logrus.WithFields(logrus.Fields{"pkg": "ebs"})
)

type ebsService struct {
	metadataClient *ec2metadata.Client
	ec2Client      *ec2.EC2

	InstanceID       string
	Region           string
	AvailabilityZone string
}

func parseAwsError(err error) error {
	if err == nil {
		return nil
	}
	if awsErr, ok := err.(awserr.Error); ok {
		message := fmt.Sprintln("AWS Error: ", awsErr.Code(), awsErr.Message(), awsErr.OrigErr())
		if reqErr, ok := err.(awserr.RequestFailure); ok {
			message += fmt.Sprintln(reqErr.StatusCode(), reqErr.RequestID())
		}
		return fmt.Errorf(message)
	}
	return err
}

func NewEBSService() (*ebsService, error) {
	var err error

	s := &ebsService{}
	s.metadataClient = ec2metadata.New(nil)
	if !s.isEC2Instance() {
		return nil, fmt.Errorf("Not running on an EC2 instance")
	}

	s.InstanceID, err = s.metadataClient.GetMetadata("instance-id")
	if err != nil {
		return nil, err
	}

	s.Region, err = s.metadataClient.Region()
	if err != nil {
		return nil, err
	}

	s.AvailabilityZone, err = s.metadataClient.GetMetadata("placement/availability-zone")
	if err != nil {
		return nil, err
	}

	config := aws.NewConfig().WithRegion(s.Region)
	s.ec2Client = ec2.New(config)

	return s, nil
}

func (s *ebsService) isEC2Instance() bool {
	return s.metadataClient.Available()
}

func (s *ebsService) waitForVolumeCreating(volumeID string) error {
	volume, err := s.ListSingleVolume(volumeID)
	if err != nil {
		return err
	}
	for *volume.State == ec2.VolumeStateCreating {
		log.Debugf("Waiting for volume %v creating", volumeID)
		time.Sleep(time.Second)
		volume, err = s.ListSingleVolume(volumeID)
		if err != nil {
			return err
		}
	}
	if *volume.State != ec2.VolumeStateAvailable {
		return fmt.Errorf("Failed to create volume %v, ending state %v", *volume.VolumeId, *volume.State)
	}
	return nil
}

func (s *ebsService) CreateVolume(size int64, snapshotID, volumeType string) (string, error) {
	// EBS size are in GB, we would round it up
	ebsSize := size / GB
	if size%GB > 0 {
		ebsSize += 1
	}

	params := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(s.AvailabilityZone),
		Size:             aws.Int64(ebsSize),
	}
	if snapshotID != "" {
		params.SnapshotId = aws.String(snapshotID)
	}
	if volumeType != "" {
		if volumeType != "gp2" && volumeType != "io1" && volumeType != "standard" {
			return "", fmt.Errorf("Invalid volume type for EBS: %v", volumeType)
		}
		params.VolumeType = aws.String(volumeType)
	}

	ec2Volume, err := s.ec2Client.CreateVolume(params)
	if err != nil {
		return "", parseAwsError(err)
	}

	volumeID := *ec2Volume.VolumeId
	if err = s.waitForVolumeCreating(volumeID); err != nil {
		log.Debug("Failed to create volume: ", err)
		err = s.DeleteVolume(volumeID)
		if err != nil {
			log.Errorf("Failed deleting volume: %v", parseAwsError(err))
		}
		return "", fmt.Errorf("Failed creating volume with size %v and snapshot %v",
			size, snapshotID)
	}

	return volumeID, nil
}

func (s *ebsService) DeleteVolume(volumeID string) error {
	params := &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}
	_, err := s.ec2Client.DeleteVolume(params)
	return parseAwsError(err)
}

func (s *ebsService) ListSingleVolume(volumeID string) (*ec2.Volume, error) {
	params := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{
			aws.String(volumeID),
		},
	}
	volumes, err := s.ec2Client.DescribeVolumes(params)
	if err != nil {
		return nil, parseAwsError(err)
	}
	if len(volumes.Volumes) != 1 {
		return nil, fmt.Errorf("Cannot find volume %v", volumeID)
	}
	return volumes.Volumes[0], nil
}

func (s *ebsService) waitForVolumeAttaching(volumeID string) error {
	var attachment *ec2.VolumeAttachment
	volume, err := s.ListSingleVolume(volumeID)
	if err != nil {
		return err
	}
	if len(volume.Attachments) != 0 {
		attachment = volume.Attachments[0]
	} else {
		return fmt.Errorf("Attaching failed for ", volumeID)
	}

	for *attachment.State == ec2.VolumeAttachmentStateAttaching {
		log.Debugf("Waiting for volume %v attaching", volumeID)
		time.Sleep(time.Second)
		volume, err := s.ListSingleVolume(volumeID)
		if err != nil {
			return err
		}
		if len(volume.Attachments) != 0 {
			attachment = volume.Attachments[0]
		} else {
			return fmt.Errorf("Attaching failed for ", volumeID)
		}
	}
	if *attachment.State != ec2.VolumeAttachmentStateAttached {
		return fmt.Errorf("Cannot attach volume, final state %v", *attachment.State)
	}
	return nil
}

func getBlkDevList() (map[string]bool, error) {
	devList := make(map[string]bool)
	dirList, err := ioutil.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	for _, dir := range dirList {
		devList[dir.Name()] = true
	}
	return devList, nil
}

func getAttachedDev(oldDevList map[string]bool, size int64) (string, error) {
	newDevList, err := getBlkDevList()
	attachedDev := ""
	if err != nil {
		return "", err
	}
	for dev := range newDevList {
		if oldDevList[dev] {
			continue
		}
		devSizeInSectorStr, err := ioutil.ReadFile("/sys/block/" + dev + "/size")
		if err != nil {
			return "", err
		}
		devSize, err := strconv.ParseInt(strings.TrimSpace(string(devSizeInSectorStr)), 10, 64)
		if err != nil {
			return "", err
		}
		devSize *= 512
		if devSize == size {
			if attachedDev != "" {
				return "", fmt.Errorf("Found more than one device matching description, %v and %v",
					attachedDev, dev)
			}
			attachedDev = dev
		}
	}
	if attachedDev == "" {
		return "", fmt.Errorf("Cannot find a device matching description")
	}
	return attachedDev, nil
}

func (s *ebsService) getInstanceDevList() (map[string]bool, error) {
	params := &ec2.DescribeVolumesInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("attachment.instance-id"),
				Values: []*string{
					aws.String(s.InstanceID),
				},
			},
		},
	}
	volumes, err := s.ec2Client.DescribeVolumes(params)
	if err != nil {
		return nil, parseAwsError(err)
	}
	devMap := make(map[string]bool)
	for _, volume := range volumes.Volumes {
		if len(volume.Attachments) == 0 {
			continue
		}
		devMap[*volume.Attachments[0].Device] = true
	}
	return devMap, nil
}

func (s *ebsService) FindFreeDeviceForAttach() (string, error) {
	availableDevs := make(map[string]bool)
	// Recommended available devices for EBS volume from AWS website
	chars := "fghijklmnop"
	for i := 0; i < len(chars); i++ {
		availableDevs["/dev/sd"+string(chars[i])] = true
	}
	devMap, err := s.getInstanceDevList()
	if err != nil {
		return "", err
	}
	for d := range devMap {
		if _, ok := availableDevs[d]; !ok {
			continue
		}
		availableDevs[d] = false
	}
	for dev, available := range availableDevs {
		if available {
			return dev, nil
		}
	}
	return "", fmt.Errorf("Cannot find an available device for instance %v", s.InstanceID)
}

func (s *ebsService) AttachVolume(volumeID string, size int64) (string, error) {
	dev, err := s.FindFreeDeviceForAttach()
	if err != nil {
		return "", err
	}

	log.Debugf("Attaching %v to %v's %v", volumeID, s.InstanceID, dev)
	params := &ec2.AttachVolumeInput{
		Device:     aws.String(dev),
		InstanceId: aws.String(s.InstanceID),
		VolumeId:   aws.String(volumeID),
	}

	blkList, err := getBlkDevList()
	if err != nil {
		return "", err
	}

	if _, err := s.ec2Client.AttachVolume(params); err != nil {
		return "", parseAwsError(err)
	}

	if err = s.waitForVolumeAttaching(volumeID); err != nil {
		return "", err
	}

	result, err := getAttachedDev(blkList, size)
	if err != nil {
		return "", err
	}
	return result, nil
}

func (s *ebsService) waitForVolumeDetaching(volumeID string) error {
	var attachment *ec2.VolumeAttachment
	volume, err := s.ListSingleVolume(volumeID)
	if err != nil {
		return err
	}
	if len(volume.Attachments) != 0 {
		attachment = volume.Attachments[0]
	} else {
		return fmt.Errorf("Attaching failed for ", volumeID)
	}

	for *attachment.State == ec2.VolumeAttachmentStateDetaching {
		log.Debugf("Waiting for volume %v detaching", volumeID)
		time.Sleep(time.Second)
		volume, err := s.ListSingleVolume(volumeID)
		if err != nil {
			return err
		}
		if len(volume.Attachments) != 0 {
			attachment = volume.Attachments[0]
		} else {
			// Already detached
			break
		}
	}
	return nil
}

func (s *ebsService) DetachVolume(volumeID string) error {
	params := &ec2.DetachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(s.InstanceID),
	}

	if _, err := s.ec2Client.DetachVolume(params); err != nil {
		return parseAwsError(err)
	}

	return s.waitForVolumeDetaching(volumeID)
}

func (s *ebsService) waitForSnapshotComplete(snap *ec2.Snapshot) error {
	snapshot := snap
	if *snapshot.State == ec2.SnapshotStateCompleted {
		return nil
	}
	params := &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{
			{
				Name: aws.String("snapshot-id"),
				Values: []*string{
					snapshot.SnapshotId,
				},
			},
		},
		OwnerIds: []*string{
			snapshot.OwnerId,
		},
		RestorableByUserIds: []*string{
			snapshot.OwnerId,
		},
		SnapshotIds: []*string{
			snapshot.SnapshotId,
		},
	}
	for *snapshot.State == ec2.SnapshotStatePending {
		log.Debugf("Snapshot %v process %v", *snapshot.SnapshotId, *snapshot.Progress)
		time.Sleep(time.Second)
		snapshots, err := s.ec2Client.DescribeSnapshots(params)
		if err != nil {
			return parseAwsError(err)
		}
		snapshot = snapshots.Snapshots[0]
	}
	return nil
}

func (s *ebsService) CreateSnapshot(volumeID, desc string) (string, error) {
	params := &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(volumeID),
		Description: aws.String(desc),
	}
	resp, err := s.ec2Client.CreateSnapshot(params)
	if err != nil {
		return "", parseAwsError(err)
	}
	err = s.waitForSnapshotComplete(resp)
	if err != nil {
		return "", parseAwsError(err)
	}
	return *resp.SnapshotId, nil
}

func (s *ebsService) DeleteSnapshot(snapshotID string) error {
	params := &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(snapshotID),
	}
	_, err := s.ec2Client.DeleteSnapshot(params)
	return parseAwsError(err)
}
