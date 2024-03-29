package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/go-logr/logr"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.seankhliao.com/gchat"
	"go.seankhliao.com/svcrunner"
	"go.seankhliao.com/svcrunner/envflag"
	"google.golang.org/protobuf/encoding/protojson"
)

type Server struct {
	webhook gchat.WebhookClient

	log   logr.Logger
	trace trace.Tracer
}

func New(hs *http.Server) *Server {
	s := &Server{}
	hs.Handler = s
	return s
}

func (s *Server) Register(c *envflag.Config) {
	c.StringVar(&s.webhook.Endpoint, "gchat.webhook", "", "webhook endpoint for google chat")
}

func (s *Server) Init(ctx context.Context, t svcrunner.Tools) error {
	s.log = t.Log.WithName("cloudbuild-gchat")
	s.trace = otel.Tracer("cloudbuild-gchat")

	s.webhook.Client = &http.Client{
		Transport: otelhttp.NewTransport(nil),
	}
	return nil
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	log := s.log.WithName("serve")
	ctx, span := s.trace.Start(r.Context(), "serve")
	defer span.End()

	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "read request", http.StatusBadRequest)
		log.Error(err, "read request", "ctx", ctx, "http_request", r)
		return
	}
	var msg PubSubMessage
	err = json.Unmarshal(b, &msg)
	if err != nil {
		http.Error(rw, "unmarshal pubsub", http.StatusBadRequest)
		log.Error(err, "unmarshal pubsub", "ctx", ctx, "http_request", r)
		return
	}

	var build cloudbuildpb.Build
	err = protojson.Unmarshal(msg.Message.Data, &build)
	if err != nil {
		http.Error(rw, "unmarshal build", http.StatusBadRequest)
		log.Error(err, "unmarshal build", "ctx", ctx, "http_request", r)
		return
	}

	log = log.WithValues("status", build.Status, "build", build.Id)

	switch build.Status {
	case cloudbuildpb.Build_CANCELLED,
		cloudbuildpb.Build_TIMEOUT,
		cloudbuildpb.Build_FAILURE,
		cloudbuildpb.Build_SUCCESS:
		break
	default:
		rw.WriteHeader(http.StatusOK)
		log.V(1).Info("ignoring status", "ctx", ctx, "http_request", r)
		return
	}

	ctx, span = s.trace.Start(ctx, "send message")
	defer span.End()
	// status | trigger-name | repo@branch:commit
	// duration | build-log
	ghRepo := "https://github.com/seankhliao"
	repo := build.Substitutions["REPO_NAME"]
	buildDuration := build.FinishTime.AsTime().Sub(build.StartTime.AsTime())
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s | %s | ", build.Status.String(), build.Substitutions["TRIGGER_NAME"])
	fmt.Fprintf(&buf, "<%s/%[2]s|%[2]s>", ghRepo, repo)
	fmt.Fprintf(&buf, "@<%s/%s/tree/%[3]s|%[3]s>", ghRepo, repo, build.Substitutions["BRANCH_NAME"])
	fmt.Fprintf(&buf, ":<%s/%s/commit/%s|%s>", ghRepo, repo, build.Substitutions["COMMIT_SHA"], build.Substitutions["SHORT_SHA"])
	fmt.Fprintf(&buf, "\n%v | <%s|build log>", buildDuration, build.LogUrl)

	err = s.webhook.Post(ctx, gchat.WebhookPayload{
		Text: buf.String(),
	})
	if err != nil {
		http.Error(rw, "post msg", http.StatusInternalServerError)
		log.Error(err, "post chat msg", "ctx", ctx, "http_request", r)
		return
	}
	rw.WriteHeader(http.StatusOK)
	log.Info("status reported", "ctx", ctx, "http_request", r)
}

// https://cloud.google.com/pubsub/docs/reference/rest/v1/PubsubMessage
type PubSubMessage struct {
	Message struct {
		Attributes struct {
			BuildID string `json:"buildId"`
			Status  string `json:"status"`
		} `json:"attributes"`
		Data []byte `json:"data,omitempty"`
		ID   string `json:"id"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}
