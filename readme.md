# amqp 再次封装


* 简单的接口
* 自动重连


订阅

```go

mq := amqp.Dial("amqp://guest:guest@127.0.0.1:5672")
msgs := mq.Sub("myqueue", "myexchange", "routingkey").GetMessages()
for msg := range msgs {
    switch a := msg.(type) {
    case *amqp.Delivery:
        fmt.Println(string(a.Body) + "\n")
        a.Accpet(true)
    case error:
        fmt.Println(msg)
    }
}
```

发布

```go

mq := amqp.Dial("amqp://guest:guest@127.0.0.1:5672")
pub, err := mq.Pub("myexchange", "topic")
pub.Push("routingkey",[]byte("hello,world"))
```
