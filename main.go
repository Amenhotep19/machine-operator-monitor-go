package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gocv.io/x/gocv"
)

const (
	// name is a program name
	name = "machine-operator-monitor"
	// topic is MQTT topic
	topic = "machine/safety"
	// alertNotWatching contains text to display when operator is not watching machine
	alertNotWatching = "Operator not watching machine: PAUSE MACHINE!"
	// alertAngry contains text to display when operator is angry when operating machine
	alertAngry = "Operator angry at the machine: PAUSE MACHINE!"
)

var (
	// deviceID is camera device ID
	deviceID int
	// input is path to image or video file
	input string
	// faceModel is path to .bin file of face detection model
	faceModel string
	// faceConfig is path to .xml file of face detection model configuration
	faceConfig string
	// faceConfidence is confidence threshold for face detection model
	faceConfidence float64
	// sentModel is path to .bin file of sentiment detection model
	sentModel string
	// sentConfig is path to .xml file of sentiment detection model configuration
	sentConfig string
	// sentConfidence is confidence threshold for sentiment detection model
	sentConfidence float64
	// poseModel is path to .bin file of pose detection model
	poseModel string
	// sentConfig is path to .xml file of pose detection model configuration
	poseConfig string
	// poseConfidence is confidence threshold for pose detection model
	poseConfidence float64
	// backend is inference backend
	backend int
	// target is inference target
	target int
	// publish is a flag which instructs the program to publish data analytics
	publish bool
	// rate is number of seconds between analytics are collected and sent to a remote server
	rate int
)

func init() {
	flag.IntVar(&deviceID, "device", 0, "Camera device ID")
	flag.StringVar(&input, "input", "", "Path to image or video file")
	flag.StringVar(&faceModel, "face-model", "", "Path to .bin file of face detection model")
	flag.StringVar(&faceConfig, "face-config", "", "Path to .xml file of face model configuration")
	flag.Float64Var(&faceConfidence, "face-confidence", 0.5, "Confidence threshold for face detection")
	flag.StringVar(&sentModel, "sent-model", "", "Path to .bin file of sentiment detection model")
	flag.StringVar(&sentConfig, "sent-config", "", "Path to .xml file of sentiment model configuration")
	flag.Float64Var(&sentConfidence, "sent-confidence", 0.5, "Confidence threshold for sentiment detection")
	flag.StringVar(&poseModel, "pose-model", "", "Path to .bin file of pose detection model")
	flag.StringVar(&poseConfig, "pose-config", "", "Path to .xml file of pose detection model configuration")
	flag.Float64Var(&poseConfidence, "pose-confidence", 0.5, "Confidence threshold for pose detection")
	flag.IntVar(&backend, "backend", 0, "Inference backend. 0: Auto, 1: Halide language, 2: Intel DL Inference Engine")
	flag.IntVar(&target, "target", 0, "Target device. 0: CPU, 1: OpenCL, 2: OpenCL half precision, 3: VPU")
	flag.BoolVar(&publish, "publish", false, "Publish data analytics to a remote server")
	flag.IntVar(&rate, "rate", 1, "Number of seconds between analytics are sent to a remote server")
}

// Sentiment is shopper sentiment
type Sentiment int

const (
	// NEUTRAL is neutral emotion shopper
	NEUTRAL Sentiment = iota + 1
	// HAPPY is for detecting happy emotion
	HAPPY
	// SAD is for detecting sad emotion
	SAD
	// SURPRISED is for detecting neutral emotion
	SURPRISED
	// ANGRY is for detecting anger emotion
	ANGRY
	// UNKNOWN is catchall unidentifiable emotion
	UNKNOWN
)

// String implements fmt.Stringer interface for Sentiment
func (s Sentiment) String() string {
	switch s {
	case NEUTRAL:
		return "NEUTRAL"
	case HAPPY:
		return "HAPPY"
	case SAD:
		return "SAD"
	case SURPRISED:
		return "SURPRISED"
	case ANGRY:
		return "ANGRY"
	default:
		return "UNKNOWN"
	}
}

// Pose is human pose
type Pose int

const (
	// WATCHING means operator is watching machine
	WATCHING Pose = iota + 1
	// UNDEFINED is currently undefined face pose
	UNDEFINED
)

// String implements fmt.Stringer interface for Pose
func (p Pose) String() string {
	switch p {
	case WATCHING:
		return "WATCHING"
	default:
		return "UNDEFINED"
	}
}

// Perf stores inference engine performance info
type Perf struct {
	// FaceNet stores face detector performance info
	FaceNet float64
	// SnetNet stores sentiment detector performance info
	SentNet float64
	// PoseNet stores pose detector performance info
	PoseNet float64
}

// String implements fmt.Stringer interface for Perf
func (p *Perf) String() string {
	return fmt.Sprintf("Face inference time: %.2f ms, Sentiment inference time: %.2f ms, Pose inference time: %.2f ms", p.FaceNet, p.SentNet, p.PoseNet)
}

// Result is inference result
type Result struct {
	// Watching means operator is watching the machine
	Watching bool
	// Angry means operator is angry
	Angry bool
	// prevAngry means operator was angry before
	prevAngry bool
	// Alert meansh operator has been angry so an alert must be raised
	AlertAngry bool
}

// String implements fmt.Stringer interface for Result
func (r *Result) String() string {
	return fmt.Sprintf("Watching %v, Angry: %v", r.Watching, r.Angry)
}

// ToMQTTMessage turns result into MQTT message which can be published to MQTT broker
func (r *Result) ToMQTTMessage() string {
	return fmt.Sprintf("{\"Watching\":%v, \"Angry\": %v}", r.Watching, r.Angry)
}

// getPerformanceInfo queries the Inference Engine performance info and returns it as string
func getPerformanceInfo(faceNet, sentNet, poseNet *gocv.Net, poseSentChecked bool) *Perf {
	freq := gocv.GetTickFrequency() / 1000

	facePerf := faceNet.GetPerfProfile() / freq

	var posePerf, sentPerf float64
	if poseSentChecked {
		posePerf = poseNet.GetPerfProfile() / freq
		sentPerf = sentNet.GetPerfProfile() / freq
	}

	return &Perf{
		FaceNet: facePerf,
		SentNet: sentPerf,
		PoseNet: posePerf,
	}
}

// messageRunner reads data published to pubChan with rate frequency and sends them to remote analytics server
// doneChan is used to receive a signal from the main goroutine to notify the routine to stop and return
func messageRunner(doneChan <-chan struct{}, pubChan <-chan *Result, c *MQTTClient, topic string, rate int) error {
	ticker := time.NewTicker(time.Duration(rate) * time.Second)

	for {
		select {
		case <-ticker.C:
			result := <-pubChan
			_, err := c.Publish(topic, result.ToMQTTMessage())
			// TODO: decide whether to return with error and stop program;
			// For now we just signal there was an error and carry on
			if err != nil {
				fmt.Printf("Error publishing message to %s: %v", topic, err)
			}
		case <-pubChan:
			// we discard messages in between ticker times
		case <-doneChan:
			fmt.Printf("Stopping messageRunner: received stop sginal\n")
			return nil
		}
	}

	return nil
}

// detectPoseSentiment detects sentiments for each pose of the detected facesin in img regions rectangles
// It returns a map of poses with detected sentiments
func detectPoseSentiment(poseNet, sentNet *gocv.Net, img *gocv.Mat, faces []image.Rectangle) map[Pose]Sentiment {
	var watching bool
	// sentMap maps the counts of each detected face emotion
	poseSentMap := make(map[Pose]Sentiment)
	// do the sentiment detection here
	for i := range faces {
		// make sure the face rect is completely inside the main frame
		if !faces[i].In(image.Rect(0, 0, img.Cols(), img.Rows())) {
			continue
		}

		face := img.Region(faces[i])

		// propagate the detected face forward through pose network
		poseImg := gocv.NewMat()
		face.CopyTo(&poseImg)
		poseBlob := gocv.BlobFromImage(poseImg, 1.0, image.Pt(60, 60),
			gocv.NewScalar(0, 0, 0, 0), false, false)

		// run a forward pass through sentiment network
		poseNet.SetInput(poseBlob, "")
		poseRes := poseNet.Forward("")

		// TODO: do something with the result here
		fmt.Println(watching)

		// propagate the detected face forward through sentiment network
		sentImg := gocv.NewMat()
		face.CopyTo(&sentImg)
		sentBlob := gocv.BlobFromImage(sentImg, 1.0, image.Pt(64, 64),
			gocv.NewScalar(0, 0, 0, 0), false, false)

		// run a forward pass through sentiment network
		sentNet.SetInput(sentBlob, "")
		sentRes := sentNet.Forward("")

		// flatten the result from [1, 5, 1, 1] to [1, 5]
		sentRes = sentRes.Reshape(1, 5)
		// find the most likely mood in returned list of sentiments
		_, confidence, _, maxLoc := gocv.MinMaxLoc(sentRes)
		if float64(confidence) > sentConfidence {
			//TODO: do something here
			fmt.Println(confidence, maxLoc)
		}

		poseBlob.Close()
		poseRes.Close()
		sentBlob.Close()
		sentRes.Close()
	}

	return poseSentMap
}

// detectFaces detects faces in img and returns them as a slice of rectangles that encapsulates them
func detectFaces(net *gocv.Net, img *gocv.Mat) []image.Rectangle {
	// convert img Mat to 672x384 blob that the face detector can analyze
	blob := gocv.BlobFromImage(*img, 1.0, image.Pt(672, 384), gocv.NewScalar(0, 0, 0, 0), false, false)
	defer blob.Close()

	// run a forward pass through the network
	net.SetInput(blob, "")
	results := net.Forward("")
	defer results.Close()

	// iterate through all detections and append results to faces buffer
	var faces []image.Rectangle
	for i := 0; i < results.Total(); i += 7 {
		confidence := results.GetFloatAt(0, i+2)
		if float64(confidence) > faceConfidence {
			left := int(results.GetFloatAt(0, i+3) * float32(img.Cols()))
			top := int(results.GetFloatAt(0, i+4) * float32(img.Rows()))
			right := int(results.GetFloatAt(0, i+5) * float32(img.Cols()))
			bottom := int(results.GetFloatAt(0, i+6) * float32(img.Rows()))
			faces = append(faces, image.Rect(left, top, right, bottom))
		}
	}

	return faces
}

// frameRunner reads image frames from framesChan and performs face and sentiment detections on them
// doneChan is used to receive a signal from the main goroutine to notify frameRunner to stop and return
func frameRunner(framesChan <-chan *gocv.Mat, doneChan <-chan struct{}, resultsChan chan<- *Result,
	perfChan chan<- *Perf, pubChan chan<- *Result, faceNet, sentNet, poseNet *gocv.Net) error {

	for {
		select {
		case <-doneChan:
			fmt.Printf("Stopping frameRunner: received stop sginal\n")
			return nil
		case frame := <-framesChan:
			// let's make a copy of the original
			img := gocv.NewMat()
			frame.CopyTo(&img)

			// detect faces and return them
			faces := detectFaces(faceNet, &img)

			// poseSentChecked is only set to true if at least one pose sentiment has been detected
			poseSentChecked := false

			// detect sentiment from detected faces
			poseSentMap := detectPoseSentiment(poseNet, sentNet, &img, faces)

			if len(poseSentMap) != 0 {
				poseSentChecked = true
			}

			// detection result
			result := &Result{}

			// send data down the channels
			perfChan <- getPerformanceInfo(faceNet, sentNet, poseNet, poseSentChecked)
			resultsChan <- result
			if pubChan != nil {
				pubChan <- result
			}

			// close image matrices
			img.Close()
		}
	}

	return nil
}

func parseCliFlags() error {
	// parse cli flags
	flag.Parse()

	// path to face detection model can't be empty
	if faceModel == "" {
		return fmt.Errorf("Invalid path to .bin file of face detection model: %s", faceModel)
	}
	// path to face detection model config can't be empty
	if faceConfig == "" {
		return fmt.Errorf("Invalid path to .xml file of face model configuration: %s", faceConfig)
	}
	// path to sentiment detection model can't be empty
	if sentModel == "" {
		return fmt.Errorf("Invalid path to .bin file of sentiment detection model: %s", sentModel)
	}
	// path to sentiment detection model config can't be empty
	if sentConfig == "" {
		return fmt.Errorf("Invalid path to .xml file of sentiment model configuration: %s", sentConfig)
	}
	// path to pose detection model can't be empty
	if poseModel == "" {
		return fmt.Errorf("Invalid path to .bin file of pose detection model: %s", poseModel)
	}
	// path to pose detection model config can't be empty
	if poseConfig == "" {
		return fmt.Errorf("Invalid path to .xml file of pose model configuration: %s", poseConfig)
	}

	return nil
}

// NewInferModel reads DNN model and it configuration, sets its preferable target and backend and returns it.
// It returns error if either the model files failed to be read or setting the target fails
func NewInferModel(model, config string, backend, target int) (*gocv.Net, error) {
	// read in Face model and set the target
	m := gocv.ReadNet(model, config)

	if err := m.SetPreferableBackend(gocv.NetBackendType(backend)); err != nil {
		return nil, err
	}

	if err := m.SetPreferableTarget(gocv.NetTargetType(target)); err != nil {
		return nil, err
	}

	return &m, nil
}

// NewCapture creates new video capture from input or camera backend if input is empty and returns it.
// It fails with error if it either can't open the input video file or the video device
func NewCapture(input string, deviceID int) (*gocv.VideoCapture, error) {
	if input != "" {
		// open video file
		vc, err := gocv.VideoCaptureFile(input)
		if err != nil {
			return nil, err
		}

		return vc, nil
	}

	// open camera device
	vc, err := gocv.VideoCaptureDevice(deviceID)
	if err != nil {
		return nil, err
	}

	return vc, nil
}

// NewMQTTPublisher creates new MQTT client which collects analytics data and publishes them to remote MQTT server.
// It attempts to make a connection to the remote server and if successful it return the client handler
// It returns error if either the connection to the remote server failed or if the client config is invalid.
func NewMQTTPublisher() (*MQTTClient, error) {
	// create MQTT client and connect to MQTT server
	opts, err := MQTTClientOptions()
	if err != nil {
		return nil, err
	}

	// create MQTT client ad connect to remote server
	c, err := MQTTConnect(opts)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func main() {
	// parse cli flags
	if err := parseCliFlags(); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing command line parameters: %v\n", err)
		os.Exit(1)
	}

	// read in Face detection model and set its inference backend and target
	faceNet, err := NewInferModel(faceModel, faceConfig, backend, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Face detection model: %v\n", err)
		os.Exit(1)
	}

	// read in Sentiment detection model and set its inference backend and target
	sentNet, err := NewInferModel(sentModel, sentConfig, backend, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Sentiment detection model: %v\n", err)
		os.Exit(1)
	}

	// read in Pose detection model and set its inference backend and target
	poseNet, err := NewInferModel(poseModel, poseConfig, backend, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Pose detection model: %v\n", err)
		os.Exit(1)
	}

	// create new video capture
	vc, err := NewCapture(input, deviceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating new video capture: %v\n", err)
		os.Exit(1)
	}
	defer vc.Close()

	// frames channel provides the source of images to process
	framesChan := make(chan *gocv.Mat, 1)
	// errChan is a channel used to capture program errors
	errChan := make(chan error, 2)
	// doneChan is used to signal goroutines they need to stop
	doneChan := make(chan struct{})
	// resultsChan is used for detection distribution
	resultsChan := make(chan *Result, 1)
	// perfChan is used for collecting performance stats
	perfChan := make(chan *Perf, 1)
	// sigChan is used as a handler to stop all the goroutines
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill, syscall.SIGTERM)
	// pubChan is used for publishing data analytics stats
	var pubChan chan *Result
	// waitgroup to synchronise all goroutines
	var wg sync.WaitGroup

	if publish {
		p, err := NewMQTTPublisher()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create MQTT publisher: %v\n", err)
			os.Exit(1)
		}
		pubChan = make(chan *Result, 1)
		// start MQTT worker goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			errChan <- messageRunner(doneChan, pubChan, p, topic, rate)
		}()
		defer p.Disconnect(100)
	}

	// start frameRunner goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		errChan <- frameRunner(framesChan, doneChan, resultsChan, perfChan, pubChan, faceNet, sentNet, poseNet)
	}()

	// open display window
	window := gocv.NewWindow(name)
	window.SetWindowProperty(gocv.WindowPropertyAutosize, gocv.WindowAutosize)
	defer window.Close()

	// prepare input image matrix
	img := gocv.NewMat()
	defer img.Close()

	// initialize the result pointers
	result := new(Result)
	perf := new(Perf)

monitor:
	for {
		if ok := vc.Read(&img); !ok {
			fmt.Printf("Cannot read image source %v\n", deviceID)
			break
		}
		if img.Empty() {
			continue
		}

		framesChan <- &img

		select {
		case sig := <-sigChan:
			fmt.Printf("Shutting down. Got signal: %s\n", sig)
			break monitor
		case err = <-errChan:
			fmt.Printf("Shutting down. Encountered error: %s\n", err)
			break monitor
		case result = <-resultsChan:
			perf = <-perfChan
		default:
			// do nothing; just display latest results
		}
		// inference performance and print it
		gocv.PutText(&img, fmt.Sprintf("%s", perf), image.Point{0, 15},
			gocv.FontHersheySimplex, 0.5, color.RGBA{0, 0, 0, 0}, 2)
		// inference results label
		gocv.PutText(&img, fmt.Sprintf("%s", result), image.Point{0, 40},
			gocv.FontHersheySimplex, 0.5, color.RGBA{0, 0, 0, 0}, 2)
		// display alert message when operator is not watching machine
		if !result.Watching {
			gocv.PutText(&img, alertNotWatching, image.Point{0, 80},
				gocv.FontHersheySimplex, 0.5, color.RGBA{255, 0, 0, 0}, 2)
		}
		// display alert message when operator is angrily operating machine
		if result.AlertAngry {
			gocv.PutText(&img, alertAngry, image.Point{0, 80},
				gocv.FontHersheySimplex, 0.5, color.RGBA{255, 0, 0, 0}, 2)
		}
		// show the image in the window, and wait 1 millisecond
		window.IMShow(img)

		// exit when ESC key is pressed
		if window.WaitKey(1) == 27 {
			break monitor
		}
	}
	// signal all goroutines to finish
	close(doneChan)
	// wait for all goroutines to finish
	wg.Wait()
}