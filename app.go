package main

import (
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"encoding/base64"
	"encoding/json"
	"encoding/xml"

	"time"

	"github.com/Financial-Times/message-queue-go-producer/producer"
	"github.com/Financial-Times/message-queue-gonsumer/consumer"
	"github.com/gorilla/mux"
	"github.com/jawher/mow.cli"
	"github.com/kr/pretty"
	"github.com/twinj/uuid"
)

var messageProducer producer.MessageProducer
var taxonomyHandlers = make(map[string]TaxonomyService)

const messageTimestampDateFormat = "2006-01-02T15:04:05.000Z"

func main() {
	app := cli.App("V1 suggestor", "A service to read V1 metadata publish event, filter it and output UP-specific metadata to the destination queue.")
	sourceAddresses := app.Strings(cli.StringsOpt{
		Name:   "source-addresses",
		Value:  []string{},
		Desc:   "Addresses used by the queue consumer to connect to the queue",
		EnvVar: "SRC_ADDR",
	})
	sourceGroup := app.String(cli.StringOpt{
		Name:   "source-group",
		Value:  "",
		Desc:   "Group used to read the messages from the queue",
		EnvVar: "SRC_GROUP",
	})
	sourceTopic := app.String(cli.StringOpt{
		Name:   "source-topic",
		Value:  "",
		Desc:   "The topic to read the meassages from",
		EnvVar: "SRC_TOPIC",
	})
	sourceQueue := app.String(cli.StringOpt{
		Name:   "source-queue",
		Value:  "",
		Desc:   "Thew queue to read the messages from",
		EnvVar: "SRC_QUEUE",
	})
	sourceConcurrentProcessing := app.Bool(cli.BoolOpt{
		Name:   "source-concurrent-processing",
		Value:  false,
		Desc:   "Whether the consumer uses concurrent processing for the messages",
		EnvVar: "SRC_CONCURRENT_PROCESSING",
	})
	destinationAddress := app.String(cli.StringOpt{
		Name:   "destination-address",
		Value:  "",
		Desc:   "Address used by the producer to connect to the queue",
		EnvVar: "DEST_ADDRESS",
	})
	destinationTopic := app.String(cli.StringOpt{
		Name:   "destination-topic",
		Value:  "",
		Desc:   "The topic to write the concept suggestion to",
		EnvVar: "DEST_TOPIC",
	})
	destinationQueue := app.String(cli.StringOpt{
		Name:   "destination-queue",
		Value:  "",
		Desc:   "The queue used by the producer",
		EnvVar: "DEST_QUEUE",
	})

	app.Action = func() {
		srcConf := consumer.QueueConfig{
			Addrs:                *sourceAddresses,
			Group:                *sourceGroup,
			Topic:                *sourceTopic,
			Queue:                *sourceQueue,
			ConcurrentProcessing: *sourceConcurrentProcessing,
		}

		destConf := producer.MessageProducerConfig{
			Addr:  *destinationAddress,
			Topic: *destinationTopic,
			Queue: *destinationQueue,
		}

		initLogs(os.Stdout, os.Stdout, os.Stderr)
		infoLogger.Printf("[Startup] Using source configuration: %# v", pretty.Formatter(srcConf))
		infoLogger.Printf("[Startup] Using dest configuration: %# v", pretty.Formatter(destConf))

		setupTaxonomyHandlers()

		infoLogger.Printf("[Startup] Handling taxonomies:")
		for key := range taxonomyHandlers {
			infoLogger.Printf("\t %v", key)
		}

		go enableHealthChecks(srcConf, destConf)

		initializeProducer(destConf)
		readMessages(srcConf)
	}

	app.Run(os.Args)
}

func setupTaxonomyHandlers() {
	taxonomyHandlers["subjects"] = SubjectService{HandledTaxonomy: "subjects"}
	taxonomyHandlers["sections"] = SectionService{HandledTaxonomy: "sections"}
	taxonomyHandlers["topics"] = TopicService{HandledTaxonomy: "topics"}
}

func enableHealthChecks(srcConf consumer.QueueConfig, destConf producer.MessageProducerConfig) {
	healthCheck := &Healthcheck{
		client:   http.Client{},
		srcConf:  srcConf,
		destConf: destConf}
	router := mux.NewRouter()
	router.HandleFunc("/__health", healthCheck.checkHealth())
	router.HandleFunc("/__gtg", healthCheck.gtg)
	http.Handle("/", router)
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		errorLogger.Panicf("Couldn't set up HTTP listener: %v\n", err)
	}
}

func initializeProducer(config producer.MessageProducerConfig) {
	messageProducer = producer.NewMessageProducer(config)
	infoLogger.Printf("[Startup] Producer: %# v", pretty.Formatter(messageProducer))
}

func readMessages(config consumer.QueueConfig) {
	messageConsumer := consumer.NewConsumer(config, handleMessage, http.Client{})
	infoLogger.Printf("[Startup] Consumer: %# v", pretty.Formatter(messageConsumer))

	var consumerWaitGroup sync.WaitGroup
	consumerWaitGroup.Add(1)

	go func() {
		messageConsumer.Start()
		consumerWaitGroup.Done()
	}()

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	messageConsumer.Stop()
	consumerWaitGroup.Wait()
}

func handleMessage(msg consumer.Message) {
	tid := msg.Headers["X-Request-Id"]

	var metadataPublishEvent MetadataPublishEvent
	err := json.Unmarshal([]byte(msg.Body), &metadataPublishEvent)
	if err != nil {
		errorLogger.Printf("[%s] Cannot unmarshal message body:[%v]", tid, err.Error())
		return
	}

	infoLogger.Printf("[%s] Processing metadata publish event for uuid [%s]", tid, metadataPublishEvent.UUID)

	metadataXML, err := base64.StdEncoding.DecodeString(metadataPublishEvent.Value)
	if err != nil {
		errorLogger.Printf("[%s] Error decoding body for uuid:  [%s]", tid, err.Error())
		return
	}

	metadata := ContentRef{}
	err = xml.Unmarshal(metadataXML, &metadata)
	if err != nil {
		errorLogger.Printf("[%s] Error unmarshalling metadata XML: [%v]", tid, err.Error())
		return
	}

	suggestions := []suggestion{}
	for key, value := range taxonomyHandlers {
		infoLogger.Printf("[%s] Processing taxonomy [%s]", tid, key)
		suggestions = append(suggestions, value.buildSuggestions(metadata)...)
	}

	conceptSuggestion := ConceptSuggestion{UUID: metadataPublishEvent.UUID, Suggestions: suggestions}

	marshalledSuggestions, err := json.Marshal(conceptSuggestion)
	if err != nil {
		errorLogger.Printf("[%s] Error marshalling the concept suggestions: [%v]", tid, err.Error())
		return
	}

	var headers = buildConceptSuggestionsHeader(msg.Headers)
	message := producer.Message{Headers: headers, Body: string(marshalledSuggestions)}
	err = messageProducer.SendMessage(conceptSuggestion.UUID, message)
	if err != nil {
		errorLogger.Printf("[%s] Error sending concept suggestion to queue: [%v]", tid, err.Error())
	}

	infoLogger.Printf("[%s] Sent suggestion message for [%s] with message ID [%s] to queue.", tid, metadataPublishEvent.UUID, headers["Message-Id"])
}

func buildConceptSuggestionsHeader(publishEventHeaders map[string]string) map[string]string {
	return map[string]string{
		"Message-Id":        uuid.NewV4().String(),
		"Message-Type":      "concept-suggestions",
		"Content-Type":      publishEventHeaders["Content-Type"],
		"X-Request-Id":      publishEventHeaders["X-Request-Id"],
		"Origin-System-Id":  publishEventHeaders["Origin-System-Id"],
		"Message-Timestamp": time.Now().Format(messageTimestampDateFormat),
	}
}
