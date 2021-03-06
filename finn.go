package finn

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

/* TODO
- Move Unpack & Pack into worker, this will allow the worker to define its own message format.
	- Put default Unpack & Pack (msgpack for now) into BaseWorker.
- Write tests
- Code review
- Clean up Start() and Runner{} (and rest of code?)
- Create a Kafka queue implementation, based on sarama
	- Set consumer group via config
	- Set partition via config
*/

var runner = Runner{}

// AddWorker registers a worker with Finn
func AddWorker(worker GenericWorker) error {
	if runner.started {
		return fmt.Errorf("Cannot add a worker after Finn has started.")
	}

	if runner.workers == nil {
		runner.workers = make(map[string]GenericWorker)
	}

	runner.workers[worker.TopicName()] = worker

	return nil
}

// SetQueue sets the queue and queue configuration that Finn will use
func SetQueue(userQueue GenericQueue, userConfig QueueConfig) error {
	if runner.started {
		return fmt.Errorf("Cannot set the queue after Finn has started.")
	}

	runner.queue = userQueue
	runner.config = userConfig

	return nil
}

// Listen boots Finn up and begins listening for work on the queue
func Listen() {
	LogInfoColour("Starting Finn up.")
	defer LogInfoColour("Shutting Finn down.")

	// Use optimal number of cores
	cpus := runtime.NumCPU()
	runtime.GOMAXPROCS(cpus)

	// Initialize runner
	if err := runner.Initialize(); err != nil {
		LogError(err)
		return
	}

	// Shut things down properly
	defer runner.Close()

	// Set up signal channel, safely shutdown on detection of a signal
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	// Connect to topics for workers to listen on, then
	// get a channel of messages from each topic
	streams := make(map[string]<-chan []byte)
	for _, worker := range runner.workers {

		topic, err := runner.queue.NewTopic(worker.TopicName())
		if err != nil {
			LogError(err)
			return
		}

		stream, err := topic.Stream()
		if err != nil {
			LogError(err)
			return
		}

		LogInfo(fmt.Sprintf("Registered worker: %s", worker.Name()))

		streams[worker.TopicName()] = stream
	}

	// Multiplex all topic streams into one channel
	jobs := multiplex(streams)

	waitGroup := new(sync.WaitGroup)
	LogInfo("Listening for work...")

MainLoop:
	for {
		select {
		case signal := <-signalChannel:
			LogInfo(fmt.Sprintf("\nReceived signal '%v', stopping workers...", signal))
			break MainLoop
		case job, ok := <-jobs:
			if ok {
				worker, err := Unpack(job.body, runner.workers[job.topic])
				if err != nil {
					LogError(err)
				} else {
					waitGroup.Add(1)
					runner.Run(worker, waitGroup)
				}
			} else {
				LogError(fmt.Errorf("Problem with worker delivery\n"))
			}
		}
	}

	// Waiting for all goroutines/workers to finish processing before shutdown
	waitGroup.Wait()
}

// Job represents the packed job + the topic it came in on
type Job struct {
	topic string
	body  []byte
}

// multiplex takes multiple input channels and routes them to a single output channel
func multiplex(streams map[string]<-chan []byte) <-chan Job {
	jobs := make(chan Job)

	// Range over all input channels
	for topic, stream := range streams {
		// Re-declaration is necessary, otherwise goroutines will all share the same variables
		topic := topic
		stream := stream
		go func() {
			// Range over messages from input channel, outputting them as a job
			for message := range stream {
				jobs <- Job{body: message, topic: topic}
			}
		}()
	}

	return jobs
}

// Runner handles running/retrying workers
type Runner struct {
	queue   GenericQueue
	config  QueueConfig
	workers map[string]GenericWorker
	started bool
}

// Initialize sets up the worker runner
func (self *Runner) Initialize() error {

	self.started = true

	// Ensure at least one worker has been set
	if len(self.workers) < 1 {
		return fmt.Errorf("No workers have been set.")
	}

	// Default to RabbitMQ if another queue isn't passed in
	if self.queue == nil {
		LogInfo("Queue not set, defaulting to RabbitMQ.")
		self.queue = &RabbitQueue{}
	}

	if err := runner.queue.Initialize(self.config); err != nil {
		return err
	}

	return nil
}

// Run handles the run and retry logic for a single job
func (self *Runner) Run(worker GenericWorker, waitGroup *sync.WaitGroup) {
	// No work to do
	if worker == nil {
		return
	}

	// Do the retrying logic asynchronously
	go func() {
		success := make(chan bool)

		// Run the worker
		// TODO look into panics
		func() {
			if duration := worker.RunDelay(); duration.Seconds() > 0 {
				LogInfo(fmt.Sprintf("%s: Delaying job for %s", worker.Name(), duration.String()))
				time.Sleep(duration)
			}

			worker.IncreaseAttempts()

			LogInfo(fmt.Sprintf("%s: Running job [%d of %d]", worker.Name(), worker.Attempts(), worker.MaxAttempts()))
			err, retry := worker.Run()
			if err != nil && retry {
				success <- false
				return
			} else if err != nil {
				LogError(fmt.Errorf("%s - %s\n", worker.Name(), err.Error()))
			}

			waitGroup.Done()
			success <- true
		}()

		// Waiting for the result of the worker
		var wasSuccess bool
		wasSuccess = <-success

		// Retry the worker if it failed
		if !wasSuccess {
			func() {
				self.Retry(worker, waitGroup)
			}()
		}
	}()
}

// Retry handles the logic for retrying a job
func (self *Runner) Retry(worker GenericWorker, waitGroup *sync.WaitGroup) {
	if worker.Attempts() >= worker.MaxAttempts() {
		LogError(fmt.Errorf("%s - Max attempts (%d) for job reached, failed to process job.", worker.Name(), worker.Attempts()))
		waitGroup.Done()
		return
	}

	worker.SetStartStamp(worker.NextStartStamp(worker.RetryDelaySeconds()))

	LogError(fmt.Errorf("%s: Retrying event [%d of %d]", worker.Name(), worker.Attempts()+1, worker.MaxAttempts()))
	self.Run(worker, waitGroup)
}

// Close shuts down the Runner and underlying queue
func (self *Runner) Close() {
	self.queue.Close()
}
