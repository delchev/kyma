package handlers

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gofrs/uuid"
	api "github.com/kyma-project/kyma/components/event-bus/api/publish"
	"github.com/kyma-project/kyma/components/event-bus/cmd/event-bus-publish-knative/publisher"
	"github.com/kyma-project/kyma/components/event-bus/cmd/event-bus-publish-knative/validators"
	knative "github.com/kyma-project/kyma/components/event-bus/internal/knative/util"
	"github.com/kyma-project/kyma/components/event-bus/internal/publish"
	"github.com/kyma-project/kyma/components/event-bus/internal/trace"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

var (
	defaultChannelNamespace = knative.GetDefaultChannelNamespace()
)

type Message struct {
	Headers map[string]string `json:"headers,omitempty"`

	Payload api.AnyValue `json:"payload,omitempty"`
}

// WithRequestSizeLimiting creates a new request size limiting HandlerFunc
func WithRequestSizeLimiting(next http.HandlerFunc, limit int64) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(rw, r.Body, limit)
		next.ServeHTTP(rw, r)
	}
}

func KnativePublishHandler(knativeLib *knative.KnativeLib, knativePublisher *publisher.KnativePublisher, tracer *trace.Tracer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// init the trace span and context
		traceSpan, traceContext := initTrace(r, tracer)
		defer trace.FinishSpan(traceSpan)

		// handle the knativeLib publish request
		message, channelName, namespace, err := handleKnativePublishRequest(w, r, knativeLib, knativePublisher, traceContext)

		// check if the publish request was successful
		if err != nil {
			// add an error span for the failure
			trace.TagSpanAsError(traceSpan, err.Message, err.MoreInfo)
			return
		}

		// send success response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		publishResponse := &api.PublishResponse{EventID: message.Headers[trace.HeaderEventID]}
		if err := json.NewEncoder(w).Encode(*publishResponse); err != nil {
			log.Printf("failed to send response back: %v", err)
		} else {
			log.Printf("publish success to the knative channel '%v' in namespace '%v'", *channelName, *namespace)
		}

		// add span tags for the message properties
		addSpanTagsForMessage(traceSpan, message)
	}
}

func handleKnativePublishRequest(w http.ResponseWriter, r *http.Request, knativeLib *knative.KnativeLib, knativePublisher *publisher.KnativePublisher, context *api.TraceContext) (*Message, *string, *string, *api.Error) {
	// validate the http request
	publishRequest, err := validators.ValidateRequest(r)
	if err != nil {
		log.Printf("validate request failed: %v", err)
		_ = publish.SendJSONError(w, err)
		return nil, nil, nil, err
	}

	// set source-id from the headers if missing in the payload
	if hasSourceID := setSourceID(publishRequest, &r.Header); !hasSourceID {
		err = api.ErrorResponseMissingFieldSourceId()
		log.Printf("source-id missing: %v", err)
		_ = publish.SendJSONError(w, err)
		return nil, nil, nil, err
	}

	// validate the publish request
	if err = api.ValidatePublish(publishRequest); err != nil {
		log.Printf("validate publish failed: %v", err)
		_ = publish.SendJSONError(w, err)
		return nil, nil, nil, err
	}

	// generate event-id if there is none
	if len(publishRequest.EventID) == 0 {
		eventID, errEventID := generateEventID()
		if errEventID != nil {
			err = api.ErrorResponseInternalServer()
			log.Printf("EventID generation failed: %v", err)
			publish.SendJSONError(w, err)
			return nil, nil, nil, err
		}
		publishRequest.EventID = eventID
	}

	// build the message from the publish-request and the trace-context
	message := buildMessage(publishRequest, context)

	// marshal the message
	messagePayload, errMarshal := json.Marshal(message.Payload)
	if errMarshal != nil {
		log.Printf("marshal message failed: %v", errMarshal.Error())
		err = api.ErrorResponseInternalServer()
		_ = publish.SendJSONError(w, err)
		return nil, nil, nil, err
	}

	// publish the message
	channelName := knative.GetChannelName(&publishRequest.SourceID, &publishRequest.EventType, &publishRequest.EventTypeVersion)
	err = (*knativePublisher).Publish(knativeLib, &channelName, &defaultChannelNamespace, &message.Headers, &messagePayload)
	if err != nil {
		log.Printf("publish message failed: %v", err)
		_ = publish.SendJSONError(w, err)
		return nil, nil, nil, err
	}

	return message, &channelName, &defaultChannelNamespace, nil
}

func initTrace(r *http.Request, tracer *trace.Tracer) (span *opentracing.Span, context *api.TraceContext) {
	if (*tracer).Started() {
		spanContext := trace.ReadTraceHeaders(&r.Header)
		span = trace.StartSpan(spanContext, &(*tracer).Options().OperationName, ext.SpanKindProducer)
		context = trace.WriteSpan(span)
	}
	return span, context
}

func setSourceID(publishRequest *api.PublishRequest, header *http.Header) bool {
	// source-id in the request body
	if len(publishRequest.SourceID) > 0 {
		return true
	}

	// source-id in the request headers
	if sourceId := header.Get(api.HeaderSourceId); len(sourceId) > 0 {
		publishRequest.SourceID = sourceId
		publishRequest.SourceIdFromHeader = true
		return true
	}

	// source-id is missing
	return false
}

func generateEventID() (string, error) {
	uid, err := uuid.NewV4()
	return uid.String(), err
}

func buildMessage(publishRequest *api.PublishRequest, traceContext *api.TraceContext) *Message {

	headers := make(map[string]string)
	headers[trace.HeaderSourceID] = publishRequest.SourceID
	headers[trace.HeaderEventType] = publishRequest.EventType
	headers[trace.HeaderEventTypeVersion] = publishRequest.EventTypeVersion
	headers[trace.HeaderEventID] = publishRequest.EventID
	headers[trace.HeaderEventTime] = publishRequest.EventTime
	if traceContext != nil {
		for k, v := range *traceContext {
			headers[k] = v
		}
	}

	message := &Message{
		Headers: headers,
		Payload: publishRequest.Data,
	}

	return message
}

func addSpanTagsForMessage(publishSpan *opentracing.Span, message *Message) {
	tags := trace.CreateTraceTagsFromMessageHeader(message.Headers)
	trace.SetSpanTags(publishSpan, &tags)
}
