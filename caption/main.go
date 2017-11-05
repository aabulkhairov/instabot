package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/go-redis/redis"
	"github.com/spf13/viper"
)

const envWorkerRedisAddr = "WORKER_REDIS_ADDR"
const envWorkerRedisDb = "WORKER_REDIS_DB"
const envWorkerRedisPasswd = "WORKER_REDIS_PASSWD"
const envWorkerRedisChannel = "WORKER_REDIS_CHANNEL"
const envWorkerCaptionApiUrl = "WORKER_CAPTION_URL"
const envWorkerCaptionApiKey = "WORKER_CAPTION_KEY"

type Worker struct {
	redis *redis.Client
	config *workerConfig
}

type workerConfig struct {
	captionApi struct{
		url string
		key string
	}
	redis struct{
		channel string
		addr string
		passwd string
		db int
	}
}

type PhotoMetadata struct {
	ChatId		int64  `json:"chat_id"`
	PhotoUrl  string `json:"photo_url"`
	Caption   string `json:"caption"`
	StyledUrl string `json:"styled_url"`
	Published bool   `json:"published"`
	PhotoId 	string `json:"photo_id"`
}

type CaptionApiResponse struct {
	Output string
	Job_id int
	Err string
}

func main() {
	worker := NewWorker()
	worker.Start()
}

func NewWorker() *Worker {
	var worker Worker

	worker.config = config()

	if len(worker.config.captionApi.key) == 0 {
		log.Fatal("[FATAL] Couldn't create worker due to laking caption api key")
	}

	worker.redis = redis.NewClient(&redis.Options{
		Addr: worker.config.redis.addr,
		Password: worker.config.redis.passwd,
		DB: worker.config.redis.db,
	})
	
	worker.setupRedis()

	return &worker
}

func (worker Worker) Start() {

}

func config() *workerConfig {
	viper.AutomaticEnv()
	viper.SetDefault(envWorkerCaptionApiUrl, "https://api.deepai.org/api/neuraltalk")
	viper.SetDefault(envWorkerRedisAddr, "localhost:6379")
	viper.SetDefault(envWorkerRedisPasswd, "")
	viper.SetDefault(envWorkerRedisChannel, "queue")
	viper.SetDefault(envWorkerRedisDb, 0)

	conf := &workerConfig{}

	conf.captionApi.url = viper.GetString(envWorkerCaptionApiUrl)
	conf.captionApi.key = viper.GetString(envWorkerCaptionApiKey)

	conf.redis.addr = viper.GetString(envWorkerRedisAddr)
	conf.redis.passwd = viper.GetString(envWorkerRedisPasswd)
	conf.redis.channel = viper.GetString(envWorkerRedisChannel)
	conf.redis.db = viper.GetInt(envWorkerRedisDb)

	return conf
}

func (worker Worker) setupRedis() {
	pong, err := worker.redis.Ping().Result()

	if err != nil {
		log.Printf("[ERROR] Couldn't ping redis server %s", err)
	} else {
		log.Printf("[DEBUG] got pong from redis %v", pong)
	}

	pubsub := worker.redis.Subscribe(worker.config.redis.channel)
	ch := pubsub.Channel()

	subscr, err := pubsub.ReceiveTimeout(time.Second*time.Duration(10))

	if err != nil {
		log.Printf("[ERROR] Couldn't subscribe to redis channel %s: %s",
			worker.config.redis.channel, err)
	}

	log.Printf("[DEBUG] subscribed to redis channel %s: %v",
		worker.config.redis.channel, subscr)

	for message := range ch {
		go worker.handleRedis(message)
	}
}

func (worker Worker) handleRedis(message *redis.Message) {
	log.Printf("[DEBUG] Got message from redis channel %s: %v",
		worker.config.redis.channel, message)

	var metadata PhotoMetadata
	err := json.Unmarshal([]byte(message.Payload), &metadata)

	if err != nil {
		log.Printf("[ERROR] Couldn't decode JSON metadata, %s", message.Payload)
	}

	log.Printf("[DEBUG] Got metadata from message %v", metadata)

	if len(metadata.Caption) == 0 {
		//TODO get hget data from redis, do no trust update message
		caption, err := worker.process(metadata)

		if err != nil {
			log.Printf("[ERROR] Couldn't get caption from API: %s", err)
			return
		}

		metadata.Caption = caption

		_, err = worker.redis.HSet(metadata.PhotoId, "caption", caption).Result()

		if err != nil {
			log.Printf("[ERROR] Couldn't set caption in redis for %s: %s",
				metadata.PhotoId, err)
		}

		meta, err := json.Marshal(&metadata)

		if err != nil {
			log.Printf("[ERROR] Couldn't encode JSON: %s", err)
		}
		_, err = worker.redis.Publish(worker.config.redis.channel, meta).Result()

		if err != nil {
			log.Printf("[ERROR] Couldn't publish photo metadata to redis channel: %s",
				worker.config.redis.channel, err)
		}
	}
}

func (worker Worker) process(metadata PhotoMetadata) (string, error) {
	var postData bytes.Buffer

	resp := getPhoto(metadata.PhotoUrl)

	w := multipart.NewWriter(&postData)

	fw, err := w.CreateFormFile("image", "file.jpg")

	if err != nil {
		log.Printf("[ERROR] Couldn't create image form field: %s", err)
	}

	_, err = io.Copy(fw, resp.Body)

	if err != nil {
		log.Printf("[ERROR] Couldn't write image to field: %s", err)
	}

	resp.Body.Close()
	w.Close()

	req, err := http.NewRequest("POST", worker.config.captionApi.url, &postData)

	if err != nil {
		log.Printf("[ERROR] Couldn't create http request: %s", err)
	}

	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Api-Key", worker.config.captionApi.key)

	client := &http.Client{}

	res, err := client.Do(req)

	if err != nil {
		log.Printf("[ERROR] Couldn't make a request to caption api: %s", err)
	}

	var captionResponse CaptionApiResponse

	err = json.NewDecoder(res.Body).Decode(&captionResponse)

	if err != nil {
		log.Printf("[ERROR] Couldn't read response from api: %s", err)
	}

	defer res.Body.Close()

	var captionErr error

	if len(captionResponse.Err) != 0 {
		captionErr = errors.New(captionResponse.Err)
	} else {
		captionErr = nil
	}

	return captionResponse.Output, captionErr
}


func getPhoto(uri string) * http.Response {
	parsed, err := url.Parse(uri)

	if parsed.Host == "" || err != nil {
		log.Printf("[ERROR] Incorrect photo url provided: %s", uri)
	}

	resp, err := http.Get(uri)

	if err != nil {
		log.Printf("[ERROR] Could not get the photo by %s", uri)
	}

	return resp
}
