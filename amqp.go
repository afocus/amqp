package amqp

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

type Client struct {
	dsn string
}

type Session struct {
	ch   *amqp.Channel
	conn *amqp.Connection
}

func Dial(dsn string) *Client {
	clt := &Client{
		dsn: dsn,
	}
	return clt
}

type Delivery struct {
	amqp.Delivery
}

type Publishing amqp.Publishing

func (d Delivery) GetBody() []byte {
	return d.Body
}

func (d Delivery) Accpet(flag bool) error {
	if flag {
		return d.Ack(false)
	}
	return d.Nack(false, true)
}

func (clt *Client) getSession() (*Session, error) {
	config := amqp.Config{
		Properties: amqp.NewConnectionProperties(),
	}
	name, _ := os.Hostname()
	config.Properties.SetClientConnectionName(name)
	c, err := amqp.DialConfig(clt.dsn, config)
	if err != nil {
		return nil, err
	}
	ch, err := c.Channel()
	if err != nil {
		c.Close()
		c = nil
		return nil, err
	}
	return &Session{ch: ch, conn: c}, err
}

// AutoReconnecter 重连实现接口
// github.com/streadway/amqp 不自带重连 这里需要实现下
type AutoReconnecter interface {
	Reconnect() (*amqp.Channel, error)
}

type Exchange struct {
	Name       string
	RoutingKey string
}
type Sub struct {
	clt       *Client
	qos       int
	exchanges []Exchange
	queue     string
	msgchan   chan interface{}
	// 临时  durable:false, autodelete:true
	temp bool
	// 排他 exclusive:true
	exclusive   bool
	idleTimeout time.Duration
	_started    int32
}

func (clt *Client) Sub(queue, exchange, routing string) *Sub {
	return clt.Subs(queue, []Exchange{{Name: exchange, RoutingKey: routing}})
}

func (clt *Client) Subs(queue string, exchanges []Exchange) *Sub {
	rev := &Sub{
		exchanges:   exchanges,
		queue:       queue,
		clt:         clt,
		msgchan:     make(chan interface{}),
		idleTimeout: time.Hour,
	}
	return rev
}

func (sub *Sub) SetTemp(v bool) {
	sub.temp = true
}

func (sub *Sub) SetExclusive(v bool) {
	sub.exclusive = v
}

func (sub *Sub) SetIdleTimeout(d time.Duration) {
	sub.idleTimeout = d
}

func (sub *Sub) reconnect() {
	for {
		sess, err := sub.clt.getSession()
		if err == nil {
			err = sub.bind(sess.ch)
			sess.conn.Close()
		}
		if err != nil {
			sub.msgchan <- err
		}
		time.Sleep(time.Second * 2)
	}
}

func (sub *Sub) Qos(count int) {
	if count > 0 {
		sub.qos = count
	}
}

func (sub *Sub) GetMessages() <-chan interface{} {
	if atomic.CompareAndSwapInt32(&sub._started, 0, 1) {
		go sub.reconnect()
	}
	return sub.msgchan
}

var errConsumeAutoClose = errors.New("consume auto close")

func (sub *Sub) bind(ch *amqp.Channel) error {
	defer ch.Close()
	// 声明队列，如果队列不存在则创建
	// 默认durable=true 持久化存储
	var autodel bool
	var durable = true
	if sub.temp {
		autodel = true
		durable = false
	}
	if _, err := ch.QueueDeclare(
		sub.queue, durable, autodel, sub.exclusive, false, nil,
	); err != nil {
		return err
	}
	for _, ext := range sub.exchanges {
		// 绑定队列到交换机 以便从指定交换机获取数据
		if err := ch.QueueBind(
			sub.queue, ext.RoutingKey, ext.Name, false, nil,
		); err != nil {
			return err
		}
	}
	if sub.qos > 0 {
		_ = ch.Qos(sub.qos, 0, false)
	}
	consumerName, _ := os.Hostname()
	if consumerName == "" {
		consumerName = os.Args[0]
	}
	msgs, err := ch.Consume(sub.queue, consumerName, false, false, false, false, nil)
	if err != nil {
		return err
	}
	if sub.idleTimeout <= 0 {
		for msg := range msgs {
			sub.msgchan <- &Delivery{msg}
		}
		return errConsumeAutoClose
	} else {
		delay := time.NewTimer(sub.idleTimeout)
		defer delay.Stop()
		for {
			delay.Reset(sub.idleTimeout)
			select {
			case msg, ok := <-msgs:
				if !ok {
					return errConsumeAutoClose
				}
				sub.msgchan <- &Delivery{msg}
			case <-delay.C:
				// 因为exchange被删或者其他并不会触发 导致一直获取不到消息
				return nil
			}
		}
	}
}

type Pub struct {
	clt      *Client
	session  chan *Session
	exchange string
	kind     string
}

func (clt *Client) Pub(exchange, kind string) (*Pub, error) {
	maxidle := 3
	pub := &Pub{
		clt:      clt,
		session:  make(chan *Session, maxidle),
		exchange: exchange,
		kind:     kind,
	}
	// init one
	{
		s, err := pub.reconnect()
		if err != nil {
			return nil, err
		}
		pub.putSession(s)
	}

	return pub, nil
}

func (pub *Pub) reconnect() (*Session, error) {
	sess, err := pub.clt.getSession()
	if err != nil {
		return nil, err
	}
	if err = sess.ch.ExchangeDeclare(
		pub.exchange, pub.kind, true, false, false, false, nil); err != nil {
		sess.conn.Close()
		sess = nil
	}
	return sess, err
}

func (pub *Pub) push(ctx context.Context, exchange, routing string, data Publishing) error {
	var ses *Session
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s := <-pub.session:
		ses = s
	case <-time.After(time.Second * 3):
		s, err := pub.reconnect()
		if err != nil {
			return err
		}
		ses = s
	}
	if err := pub.pushaction(ctx, ses, exchange, routing, data); err != nil {
		ses.ch.Close()
		ses.conn.Close()
		return err
	}
	pub.putSession(ses)
	return nil
}

func (pub *Pub) Push(routing string, data []byte) error {
	return pub.push(context.Background(), pub.exchange, routing, Publishing{Body: data})
}

func (pub *Pub) PushToQueue(queue string, data Publishing) error {
	return pub.push(context.Background(), "", queue, data)
}

func (pub *Pub) PubPlus(routing string, data Publishing) error {
	return pub.PubPlusWithContext(context.Background(), routing, data)
}

func (pub *Pub) PubPlusWithContext(ctx context.Context, routing string, data Publishing) error {
	return pub.push(ctx, pub.exchange, routing, data)
}

func (pub *Pub) putSession(s *Session) {
	select {
	case pub.session <- s:
	default:
		s.ch.Close()
		s.conn.Close()
	}
}

func (pub *Pub) pushaction(ctx context.Context, s *Session, exchange, routing string, data Publishing) error {
	if data.Headers == nil {
		data.Headers = amqp.Table{}
	}
	data.DeliveryMode = amqp.Persistent
	return s.ch.PublishWithContext(
		ctx,
		exchange, routing, false, false, amqp.Publishing(data),
	)
}
