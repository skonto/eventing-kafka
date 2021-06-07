/*
Copyright 2021 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dispatcher

import (
	"context"
	"errors"
	"github.com/cloudevents/sdk-go/v2/binding/buffering"
	"knative.dev/eventing/pkg/kncloudevents"
	nethttp "net/http"

	"github.com/Shopify/sarama"
	protocolkafka "github.com/cloudevents/sdk-go/protocol/kafka_sarama/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"go.uber.org/zap"
	"knative.dev/eventing-kafka/pkg/common/consumer"
	"knative.dev/eventing-kafka/pkg/common/tracing"
	eventingchannels "knative.dev/eventing/pkg/channel"
)

type consumerMessageHandler struct {
	logger            *zap.SugaredLogger
	sub               Subscription
	dispatcher        *eventingchannels.MessageDispatcherImpl
	kafkaSubscription *KafkaSubscription
	consumerGroup     string
	reporter eventingchannels.StatsReporter
	channelNs string
}

var _ consumer.KafkaConsumerHandler = (*consumerMessageHandler)(nil)

func (c consumerMessageHandler) GetConsumerGroup() string {
	return c.consumerGroup
}

func (c consumerMessageHandler) SetReady(partition int32, ready bool) {
	c.kafkaSubscription.SetReady(c.sub.UID, partition, ready)
}

func (c consumerMessageHandler) Handle(ctx context.Context, consumerMessage *sarama.ConsumerMessage) (bool, error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Warn("Panic happened while handling a message",
				zap.String("topic", consumerMessage.Topic),
				zap.Any("panic value", r),
			)
		}
	}()
	message := protocolkafka.NewMessageFromConsumerMessage(consumerMessage)
	if message.ReadEncoding() == binding.EncodingUnknown {
		return false, errors.New("received a message with unknown encoding")
	}

	c.logger.Debug("Going to dispatch the message",
		zap.String("topic", consumerMessage.Topic),
		zap.String("subscription", c.sub.String()),
	)

	ctx, span := tracing.StartTraceFromMessage(c.logger, ctx, message, consumerMessage.Topic)
	defer span.End()

	te := kncloudevents.TypeExtractorTransformer("")

	bufferedMessage, err := buffering.CopyMessage(ctx, message, &te)
	args := eventingchannels.ReportArgs{
		Ns: c.channelNs,
		EventType: string(te),
	}

	// We don't need the original message anymore
	_ = message.Finish(nil)
	dispatchExecutionInfo, dispatchErr := c.dispatcher.DispatchMessageWithRetries(
		ctx,
		bufferedMessage,
		nil,
		c.sub.Subscriber,
		c.sub.Reply,
		c.sub.DeadLetter,
		c.sub.RetryConfig,
	)

	err = parseDispatchResultAndReportMetrics(dispatchExecutionInfo, c.reporter, args,  dispatchErr)

	// NOTE: only return `true` here if DispatchMessage actually delivered the message.
	return err == nil, err
}

func parseDispatchResultAndReportMetrics(info *eventingchannels.DispatchExecutionInfo , reporter eventingchannels.StatsReporter, reportArgs eventingchannels.ReportArgs, dispatchErr error) error {
	if info != nil && info.Time > eventingchannels.NoDuration {
		if info.ResponseCode > eventingchannels.NoResponse {
			_ = reporter.ReportEventDispatchTime(&reportArgs, info.ResponseCode, info.Time)
		} else {
			_ = reporter.ReportEventDispatchTime(&reportArgs, nethttp.StatusInternalServerError, info.Time)
		}
	}
	err := dispatchErr
	if err != nil {
		eventingchannels.ReportEventCountMetricsForDispatchError(err, reporter, &reportArgs)
	} else if info != nil {
		_ = reporter.ReportEventCount(&reportArgs, info.ResponseCode)
	}
	return err
}
