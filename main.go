package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/goburrow/modbus"
	"github.com/hyperboloide/lk"
	logs "github.com/sirupsen/logrus"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type Message struct {
	Channel string `json:"channel"`
	Stime   string `json:"stime"`
	Value1  string `json:"value1"`
	Value2  string `json:"value2"`
}

var (
	log_level       = flag.String("log", "error", "Select Log Level info|debug|error|fatal")
	config_file     = flag.String("config", "ccmmbclient.config", "Select config file name")
	simulation_mode = flag.Bool("s", false, "Simulation mode")

	configfile string
	testMode   bool = false

	config        map[string]interface{}
	Device_Serial string

	handler *modbus.TCPClientHandler
	client  modbus.Client

	mqttclient mqtt.Client
	bufmemory  = make(chan string, 10)
)

type Ambient struct {
	Temperature float64 `json:"temp"`
	Humidity    float64 `json:"humidity"`
}

func main() {
	flag.Parse()

	logginglevel := *log_level
	logs.Println("logging level ", logginglevel)
	logs.SetLevel(logs.ErrorLevel)

	switch logginglevel {
	case "info":
		logs.SetLevel(logs.InfoLevel)
	case "debug":
		logs.SetLevel(logs.DebugLevel)
	case "fatal":
		logs.SetLevel(logs.FatalLevel)
	default:
		logs.SetLevel(logs.ErrorLevel)
	}

	logs.Debug("Config File ", *config_file)
	if len(*config_file) > 0 {
		configfile = *config_file
	}

	testMode = *simulation_mode

	logs.Debug("Config", configfile)

	if len(configfile) > 0 {
		getConfig()
	} else {
		logs.Fatal("The config file is not correct.")
	}

	if runtime.GOOS == "linux" {
		if checkLicense() == false {
			logs.Fatal("The license is invalid.")
		}
	}

	conMQTTBrocker()
	mqttSubscribe()
	defer func() {
		logs.Debug("Close Program")
		mqttclient.Disconnect(250)
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt)

	//logs.Debugln("Context timeout ")
	//// add an arbitrary timeout to demonstrate how to stop a subscription
	//// with a context.
	//d := 10000 * time.Second
	//ctx, cancel := context.WithTimeout(context.Background(), d)
	//defer cancel()
	logs.Debugln("Context with cancel ")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopSignal bool = false
	go func() {
		<-signalCh
		println("signal cancel")
		stopSignal = true
		cancel()
	}()

	for {

		// Modbus TCP
		logs.Debug("Connect Modbus TCP Server")
		runMBClient()

		client = modbus.NewClient(handler)

		wg := new(sync.WaitGroup)
		wg.Add(1)

		go func() {
			defer func() {
				logs.Debugln("Stopping Modbus Service")
				handler.Close()
				wg.Done()
			}()
			logs.Debugln("Start Modbus Service")
			//Sensor Read
			sendModbusData(ctx)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			logs.Debug("Go Publish")
			publishData(ctx)
		}()

		wg.Wait()

		if stopSignal {
			logs.Debugln("Stop Signal...")
			break
		}
		time.Sleep(1000 * time.Millisecond)

	}

}

func sendModbusData(ctx context.Context) {

	command := fmt.Sprintf("%v", config["mbCommand"])
	addr, _ := strconv.Atoi(fmt.Sprintf("%v", config["mbStart"]))
	mbLength, _ := strconv.Atoi(fmt.Sprintf("%v", config["mbLength"]))
	lag, err := time.ParseDuration(fmt.Sprintf("%v", config["task_update_interval_ms"]))
	if err != nil {
		lag, _ = time.ParseDuration("1000ms")
	}
	ticker := time.NewTicker(lag)
	for {

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			results := make([]byte, mbLength*2)
			if command == "ReadInput" {

				results, err = client.ReadInputRegisters(uint16(addr), uint16(mbLength))
				if err != nil || results == nil {
					logs.Error("Read InputRegisters Modbus Error : ", err)
					return
				}
			} else {
				results, err = client.ReadHoldingRegisters(uint16(addr), uint16(mbLength))
				if err != nil || results == nil {
					logs.Error("Read InputRegisters Modbus Error : ", err)
					return
				}
			}
			wdata := make([]byte, 2)
			copy(wdata[0:2], results[0:2])
			m_value := binary.BigEndian.Uint16(wdata)

			copy(wdata[0:2], results[2:4])
			m_value2 := binary.BigEndian.Uint16(wdata)

			message := &Message{
				Channel: "Conveyor",
				Stime:   time.Now().Format("2006-01-01 15:04:05.999"),
				Value1:  fmt.Sprintf("%v", m_value),
				Value2:  fmt.Sprintf("%v", m_value2),
			}

			msg, err := json.MarshalIndent(message, "", " ")
			if err != nil {
				log.Fatal(err)
			}

			bufmemory <- string(msg)
		}
	}

}

func randFloats(min, max float64, n int) []float64 {
	res := make([]float64, n)
	for i := range res {
		res[i] = min + rand.Float64()*(max-min)
	}
	return res
}

func runMBClient() {

	tcpDevice := fmt.Sprintf("%v", config["mbDevice"])
	handler = modbus.NewTCPClientHandler(tcpDevice)
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 1
	if logs.GetLevel() == logs.DebugLevel {
		handler.Logger = log.New(os.Stdout, "tcp: ", log.LstdFlags)
	}
	//handler.Logger = log.New(os.Stdout, "tcp: ", log.LstdFlags)
	handler.Connect()
}

var receiveSubscribe mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	logs.Debugf("TOPIC: %s\n", msg.Topic())
	logs.Debugf("MSG: %s\n", msg.Payload())

}

func mqttSubscribe() {
	var topic string = fmt.Sprintf("%v", config["mqtttopicrangeset"])

	if token2 := mqttclient.Subscribe(topic, 0, nil); token2.Wait() && token2.Error() != nil {
		fmt.Println(token2.Error())
	}
	logs.Debug("MQTT Subscribe ", topic)
}

func conMQTTBrocker() {
	var url string = fmt.Sprintf("%v", config["mqttbrocker"])
	var clientname string = fmt.Sprintf("%v", config["mqttclientname"])
	clientname = clientname + strconv.FormatInt(time.Now().Unix(), 10)
	//var topic string = fmt.Sprintf("%v", config["mqtttopic"])
	//mqtt.DEBUG = log.New(os.Stdout, "", 0)
	mqtt.ERROR = log.New(os.Stdout, "", 0)
	opts := mqtt.NewClientOptions().AddBroker(url).SetClientID(clientname)

	opts.SetKeepAlive(60 * time.Second)
	// Set the message callback handler
	opts.SetDefaultPublishHandler(receiveSubscribe)
	opts.SetPingTimeout(1 * time.Second)
	opts.SetAutoReconnect(true)

	mqttclient = mqtt.NewClient(opts)
	if token := mqttclient.Connect(); token.Wait() && token.Error() != nil {
		logs.Fatal(token.Error())
	}
	logs.Debug("MQTT Connect")
	//defer mqttclient.Disconnect(250)

	//if token := mqttclient.Subscribe(topic, 0, nil); token.Wait() && token.Error() != nil {
	//	fmt.Println(token.Error())
	//	os.Exit(1)
	//}
	//logs.Debug("MQTT Subscribe")
}

func publishData(ctx context.Context) {
	var topic string = fmt.Sprintf("%v", config["mqtttopic"])

	for {
		select {
		case <-ctx.Done():
			return
		case jdata := <-bufmemory:
			logs.Debugf("MB Data : %v", jdata)

			if mqttclient.IsConnected() {
				if token := mqttclient.Publish(topic, 0, false, jdata); token.Wait() && token.Error() != nil {
					logs.Debug("Publish Error : ", token.Error())
					continue
				}

			}
		}

	}
}

func getMacAddr(name string) (string, error) {
	ifas, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var as string
	for _, ifa := range ifas {
		if ifa.Name == name {
			as = ifa.HardwareAddr.String()
			break
		}
	}
	return as, nil
}

func checkLicense() bool {
	// license file check
	// get mac address : eth0
	mac, err := getMacAddr("eth0")
	if err != nil {
		logs.Fatal("Get MAC Addrss failed.")
	}
	logs.Infof("MAC Address : %s", mac)

	logs.Debug("Read License ")
	// license file
	byteValue, err := ioutil.ReadFile("./license/license.dat")
	if err != nil {
		logs.Fatal("Read license file error ", err)
		return false
	}

	licenseB32 := string(byteValue)

	logs.Debug("Read Public ")
	// Read Public key
	pubValue, err := ioutil.ReadFile("./license/ccmpublic.txt")
	if err != nil {
		logs.Fatal("Read Public Key file error ", err)
		return false
	}

	publicKeyBase32 := string(pubValue)

	logs.Debug("Get public ", publicKeyBase32)
	// Unmarshal the public key.
	publicKey, err := lk.PublicKeyFromB32String(publicKeyBase32)
	if err != nil {
		logs.Fatal(err)
	}

	logs.Debug("Unmarshal the customer license.")
	// Unmarshal the customer license.
	license, err := lk.LicenseFromB32String(licenseB32)
	if err != nil {
		logs.Fatal(err)
	}

	logs.Debug("validate the license signature.")
	// validate the license signature.
	if ok, err := license.Verify(publicKey); err != nil {
		logs.Fatal(err)
	} else if !ok {
		logs.Fatal("Invalid license signature")
	}

	var result map[string]interface{}

	if err := json.Unmarshal(license.Data, &result); err != nil {
		logs.Fatal(err)
	}

	Device_Serial = fmt.Sprintf("%v", result["serial"])

	tkflag := fmt.Sprintf("%v", result["company"])
	if tkflag != "turckkorea" {
		lmac := fmt.Sprintf("%v", result["mac"])

		if lmac != mac {
			logs.Fatal("Invalied license file.. contact to administrator")
		}
	}

	return true
}

func getConfig() {
	//var configfile string = "modbus_serial_client_manager.config"
	// Open our jsonFile
	jsonFile, err := os.Open(configfile)
	// if we os.Open returns an error then handle it
	if err != nil {
		logs.Fatal("Open Config File Error ", err)
		return
	}
	defer jsonFile.Close()

	byteValue, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		logs.Fatal("Read file error ", err)
		return

	}

	err = json.Unmarshal(byteValue, &config)
	if err != nil {
		logs.Fatal("Unmarshal error ", err)
		return
	}

}
