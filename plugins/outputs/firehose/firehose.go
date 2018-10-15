// Package firehose provides an output plugin for telegraf to write to Kinesis Firehose
package firehose

import (
	"log"
	"time"

	"github.com/aws/aws-sdk-go/service/firehose"
	"github.com/aws/aws-sdk-go/service/firehose/firehoseiface"
	"github.com/influxdata/telegraf"
	internalaws "github.com/influxdata/telegraf/internal/config/aws"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/serializers"
)

type (
	errorEntry struct {
		submitAttemptCount int
		batch              []*firehose.Record
	}
)

type (
	FirehoseOutput struct {
		Region    string `toml:"region"`
		AccessKey string `toml:"access_key"`
		SecretKey string `toml:"secret_key"`
		RoleARN   string `toml:"role_arn"`
		Profile   string `toml:"profile"`
		Filename  string `toml:"shared_credential_file"`
		Token     string `toml:"token"`

		DeliveryStreamName string `toml:"delivery_stream_name"`
		Debug              bool   `toml:"debug"`
		MaxSubmitAttempts  int    `toml:"max_submit_attempts"`
		BatchMetrics       bool   `toml:"batch_metrics"`

		svc         firehoseiface.FirehoseAPI
		errorBuffer []*errorEntry

		serializer serializers.Serializer
	}
)

var sampleConfig = `
  ## Amazon REGION of the AWS firehose endpoint.
  region = "us-east-2"

  ## Amazon Credentials
  ## Credentials are loaded in the following order
  ## 1) Assumed credentials via STS if role_arn is specified
  ## 2) explicit credentials from 'access_key' and 'secret_key'
  ## 3) shared profile from 'profile'
  ## 4) environment variables
  ## 5) shared credentials file
  ## 6) EC2 Instance Profile
  #access_key = ""
  #secret_key = ""
  #token = ""
  #role_arn = ""
  #profile = ""
  #shared_credential_file = ""

  ## Firehose StreamName must exist prior to starting telegraf.
  delivery_stream_name = "FirehoseName"

  ## The maximum number of times to attempt resubmitting a single metric
  max_submit_attempts = 10

  ## If multiple metrics should be added to a single firehose record up to 1000KB
  batch_metrics = false

  ## Data format to output.
  ## Each data format has its own unique set of configuration options, read
  ## more about them here:
  ## https://github.com/influxdata/telegraf/blob/master/docs/DATA_FORMATS_OUTPUT.md
  data_format = "influx"

  ## debug will show upstream aws messages.
  debug = false
`

func (f *FirehoseOutput) SampleConfig() string {
	return sampleConfig
}

func (f *FirehoseOutput) Description() string {
	return "Configuration for the AWS Firehose output."
}

func (f *FirehoseOutput) Connect() error {
	// We attempt first to create a session to Firehose using an IAMS role, if that fails it will fall through to using
	// environment variables, and then Shared Credentials.
	if f.Debug {
		log.Printf("E! firehose: Building a session for connection to Firehose in %+v", f.Region)
	}

	credentialConfig := &internalaws.CredentialConfig{
		Region:    f.Region,
		AccessKey: f.AccessKey,
		SecretKey: f.SecretKey,
		RoleARN:   f.RoleARN,
		Profile:   f.Profile,
		Filename:  f.Filename,
		Token:     f.Token,
	}
	configProvider := credentialConfig.Credentials()

	// we simply create the skeleton here. AWS doesn't attempt
	// any connection yet, so we don't know if an error will happen.
	svc := firehose.New(configProvider)
	f.svc = svc
	return nil
}

func (f *FirehoseOutput) Close() error {
	return nil
}

func (f *FirehoseOutput) SetSerializer(serializer serializers.Serializer) {
	f.serializer = serializer
}

// writeToFirehose uses the AWS GO SDK to write batched records to firehose and
// queue the failed writes to the errorBuffer of f
func writeToFirehose(f *FirehoseOutput, r []*firehose.Record, submitAttemptCount int) {
	start := time.Now()
	batchInput := &firehose.PutRecordBatchInput{}
	batchInput.SetDeliveryStreamName(f.DeliveryStreamName)
	batchInput.SetRecords(r)

	// attempt to send data to firehose
	resp, err := f.svc.PutRecordBatch(batchInput)

	// if we had a total failure, log it and enqueue the request for next time
	if err != nil {
		log.Printf("E! firehose: Unable to write to Firehose : %+v \n", err.Error())
		newErrorEntry := errorEntry{submitAttemptCount: submitAttemptCount + 1, batch: r}
		f.errorBuffer = append(f.errorBuffer, &newErrorEntry)
		return
	}
	if f.Debug {
		log.Printf("E! %+v \n", resp)
	}

	// if we have a partial failure- issue a warning and then enqueue only the messages that failed
	if *resp.FailedPutCount > 0 {
		log.Printf("W! Partial Failed Firehose PutRecordBatch")

		errorMetrics := make([]*firehose.Record, 0, *resp.FailedPutCount)
		for index, entry := range resp.RequestResponses {
			log.Printf("W! Failed Record %d ErrorCode: %s Error: %s", index, *entry.ErrorCode, *entry.ErrorMessage)
			if entry.ErrorCode != nil {
				errorMetrics = append(errorMetrics, r[index])
			}
		}

		newErrorEntry := errorEntry{submitAttemptCount: submitAttemptCount + 1, batch: errorMetrics}
		log.Printf("W! firehose: failed to write %d out of %d Telegraf records in %+v. Queuing failed metrics for later retry.\n", len(errorMetrics), len(r), time.Since(start))
		f.errorBuffer = append(f.errorBuffer, &newErrorEntry)
	} else {
		if f.Debug {
			log.Printf("D! firehose: successfully sent %d Telegraf records in %+v\n", len(r), time.Since(start))
		}
	}

}

func writeBatchRecords(f *FirehoseOutput, metrics []telegraf.Metric) error {
	var sz uint32

	r := []*firehose.Record{}
	for _, metric := range metrics {
		sz++

		values, err := f.serializer.Serialize(metric)
		if err != nil {
			return err
		}

		d := firehose.Record{
			Data: values,
		}
		if f.Debug {
			log.Println(d)
			log.Println(metric)
		}

		r = append(r, &d)

		if sz == 500 {
			// Max Messages Per PutRecordRequest is 500
			writeToFirehose(f, r, 0)
			sz = 0
			r = nil
		}
	}
	if sz > 0 {
		writeToFirehose(f, r, 0)
	}

	return nil
}

func writeBatchMetrics(f *FirehoseOutput, metrics []telegraf.Metric) error {
	var sz uint32
	var rd []byte

	r := []*firehose.Record{}
	for _, metric := range metrics {
		values, err := f.serializer.Serialize(metric)
		if err != nil {
			return err
		}

		// Firehose supports records of up to 1000kb
		if (len(rd) + len(values)) > 1000000 {
			sz++
			d := firehose.Record{
				Data: rd,
			}

			r = append(r, &d)

			if f.Debug {
				log.Println(metric)
				log.Println("Adding record of max size %d", len(rd))
				log.Println(d)
			}

			// Add current metric to next record
			rd = values
		} else {
			// Combine multiple metrics to single record data
			// Serializer already gurantees a newline after each record
			rd = append(rd[:], values[:]...)

			if f.Debug {
				log.Println(metric)
				log.Println("Length of record %d", len(rd))
			}
		}

		if sz == 4 {
			// Max Messages Per PutRecordRequest is 4mb
			if f.Debug {
				log.Println("Sending %d records to firehose", len(r))
			}
			writeToFirehose(f, r, 0)
			sz = 0
			r = nil
		}
	}
	if len(rd) > 0 {
		d := firehose.Record{
			Data: rd,
		}

		if f.Debug {
			log.Println("Adding remaining record of size %d", len(rd))
		}

		r = append(r, &d)

		sz++
	}
	if sz > 0 {
		writeToFirehose(f, r, 0)
	}

	return nil
}

// Write function is responsible for first writing the batch entries held in
// the errorBuffer of f and then writing metrics in batches of 500.
func (f *FirehoseOutput) Write(metrics []telegraf.Metric) error {
	if len(metrics) == 0 {
		return nil
	}


	errorSize := len(f.errorBuffer)

	if (errorSize > 0){
		log.Printf("I! firehose: processing %d batches with previous errors queued for retry. Max Attempts %d", errorSize, f.MaxSubmitAttempts)

		// Create a new errorBuffer otherwise writeToFirehouse keeps adding the same message on the buffer each attempt
		lastBuffer := f.errorBuffer
		f.errorBuffer = nil

		// if we have any failures from last write, we will attempt them
		// again here.  Note: we only try up to the max number of retry attempts
		// as specified by the configuration file.
		for i, entry := range lastBuffer {
			if entry.submitAttemptCount <= f.MaxSubmitAttempts {
				if f.Debug {
					log.Printf("D! firehose: -> resending failed metric batch %d of %d.\n. Attempt #%d", i+1, errorSize, entry.submitAttemptCount)
				}
				writeToFirehose(f, entry.batch, entry.submitAttemptCount)
			} else {
				log.Printf("I! firehose: -> discarded failed metric batch %d of %d. Attempt #%d", i+1, errorSize, entry.submitAttemptCount)
			}
		}
		log.Printf("I! firehose: done resending previous batches\n")
	}

	if f.Debug {
		log.Printf("Sending multiple metrics in single record %b", f.BatchMetrics)
	}

	if f.BatchMetrics {
		return writeBatchMetrics(f, metrics)
	} else {
		return writeBatchRecords(f, metrics)
	}
}

func init() {
	outputs.Add("firehose", func() telegraf.Output {
		return &FirehoseOutput{}
	})
}