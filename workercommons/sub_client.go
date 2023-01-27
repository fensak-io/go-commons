package workercommons

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
	"gocloud.dev/pubsub"
	"gocloud.dev/pubsub/azuresb"
	"gocloud.dev/pubsub/rabbitpubsub"
	"google.golang.org/protobuf/proto"
)

type SubClient[T proto.Message] struct {
	subscription *pubsub.Subscription

	logger     *zap.SugaredLogger
	receiver   *azservicebus.Receiver
	rabbitConn *amqp.Connection
}

// NewSubClient returns an initialized subscriber client for the configured broker from the given application config.
func NewSubClient[T proto.Message](logger *zap.SugaredLogger, broker *Broker, ctx context.Context) (*SubClient[T], error) {
	switch broker.Engine {
	case "azuresb":
		return newAzureSBReceiverClient[T](logger, broker, ctx)
	case "rabbitmq":
		return newRabbitMQSubscriberClient[T](logger, broker, ctx)
	}
	return nil, fmt.Errorf("Unknown engine")
}

// Close will close all the associated connections of the given publisher client.
func (clt *SubClient[T]) Close() error {
	if clt == nil {
		return nil
	}

	ctx := context.Background()

	if err := clt.subscription.Shutdown(ctx); err != nil {
		clt.logger.Errorf("Error shutting down subscription: %s", err)
		return err
	}

	if clt.receiver != nil {
		if err := clt.receiver.Close(ctx); err != nil {
			clt.logger.Errorf("Error closing Azure PubSub receiver: %s", err)
			return err
		}
	}

	if clt.rabbitConn != nil {
		if err := clt.rabbitConn.Close(); err != nil {
			clt.logger.Errorf("Error closing RabbitMQ connection: %s", err)
			return err
		}
	}

	return nil
}

// ReceiveTask will pull a task from the subscription channel and attempt to decode the received message into a task.
// Note that this will block the thread if there are no messages available in the topic.
// IMPORTANT: The caller must acknowledge the message once the task is successfully processed, either using Ack or Nack.
func (clt *SubClient[T]) ReceiveTask(ctx context.Context) (*T, *pubsub.Message, error) {
	msg, err := clt.subscription.Receive(ctx)
	if err != nil {
		// TODO: return as fatal error
		return nil, nil, err
	}

	msgType, hasType := msg.Metadata["type"]
	if !hasType {
		msg.Nack()
		return nil, nil, fmt.Errorf("Message has unknown type")
	}

	if msgType != "Task" {
		msg.Nack()
		return nil, nil, fmt.Errorf("Message has unknown type")
	}

	var task T
	if err := proto.Unmarshal(msg.Body, task); err != nil {
		msg.Nack()
		return nil, nil, err
	}
	return &task, msg, nil
}

func newAzureSBReceiverClient[T proto.Message](
	logger *zap.SugaredLogger, broker *Broker, ctx context.Context,
) (*SubClient[T], error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}

	clt, err := azservicebus.NewClient(broker.ConnectionString, cred, nil)
	if err != nil {
		return nil, err
	}

	var receiver *azservicebus.Receiver
	if broker.ServiceBusSubscriptionName == "" {
		receiverRaw, err := clt.NewReceiverForQueue(broker.TopicName, nil)
		if err != nil {
			return nil, err
		}
		receiver = receiverRaw
	} else {
		receiverRaw, err := azuresb.NewReceiver(clt, broker.TopicName, broker.ServiceBusSubscriptionName, nil)
		if err != nil {
			return nil, err
		}
		receiver = receiverRaw
	}

	subs, err := azuresb.OpenSubscription(ctx, clt, receiver, nil)
	if err != nil {
		return nil, err
	}

	return &SubClient[T]{
		subscription: subs,
		logger:       logger,
		receiver:     receiver,
	}, nil
}

func newRabbitMQSubscriberClient[T proto.Message](
	logger *zap.SugaredLogger, broker *Broker, ctx context.Context,
) (*SubClient[T], error) {
	rabbitConn, err := amqp.Dial(fmt.Sprintf("amqp://%s/", broker.ConnectionString))
	if err != nil {
		return nil, err
	}

	subs := rabbitpubsub.OpenSubscription(rabbitConn, broker.TopicName, nil)
	return &SubClient[T]{
		subscription: subs,
		logger:       logger,
		rabbitConn:   rabbitConn,
	}, nil
}
