package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/iancoleman/strcase"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
	"github.com/pkg/errors"
	"github.com/rspier/go-ecobee/ecobee"
	"gopkg.in/yaml.v3"
)

func main() {
	configFile := flag.String("config", "ecobeemetrics.yaml", "Config file")
	getPin := flag.Bool("getpin", false, "Get ecobee pin only")
	saveToken := flag.String("savetoken", "", "Ecobee code to get auth token")
	flag.Parse()

	// read config
	cf, err := os.ReadFile(*configFile)
	if err != nil {
		panic(fmt.Sprintf("Error reading config file %s: %s", *configFile, err))
	}
	var config Config
	err = yaml.Unmarshal(cf, &config)
	if err != nil {
		panic(fmt.Sprintf("Error loading config from %s: %s", *configFile, err))
	}

	if *getPin {
		pinResponse, err := ecobee.Authorize(config.Ecobee.AppId)
		if err != nil {
			log.Fatalf("Error authorizing with ecobee: %+v", errors.WithStack(err))
		}
		fmt.Printf("Ecobee PIN: %s\n", pinResponse.EcobeePin)
		fmt.Printf("Ecobee code: %s\n", pinResponse.Code)
		os.Exit(0)
	}
	if *saveToken != "" {
		err = ecobee.SaveToken(config.Ecobee.AppId, config.Ecobee.AuthCacheFile, *saveToken)
		if err != nil {
			log.Fatalf("%+v", err)
		}
		os.Exit(0)
	}

	// create ecobee client
	e := ecobee.NewClient(config.Ecobee.AppId, config.Ecobee.AuthCacheFile)
	id := config.Ecobee.ThermostatId

	// create influx client
	ic := config.InfluxDB
	influx := influxdb2.NewClient(ic.Host, ic.AuthToken)
	write := influx.WriteAPIBlocking(ic.Org, ic.Bucket)

	s, err := gocron.NewScheduler(gocron.WithLocation(time.UTC))
	if err != nil {
		log.Fatalf("Failed to start scheduler: %s", err)
	}
	_, err = s.NewJob(
		gocron.CronJob(config.Ecobee.PollCron, false),
		gocron.NewTask(run, e, write, id, config),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	)
	if err != nil {
		log.Fatalf("Failed to schedule: %+v", err)
	}
	s.Start()
	// block forever
	select {}
}

var lastRevision string

func run(e *ecobee.Client, writeApi api.WriteAPIBlocking, id string, config Config) {
	now := time.Now()

	tsm, err := e.GetThermostatSummary(
		ecobee.Selection{
			SelectionType:          "thermostats",
			SelectionMatch:         id,
			IncludeEquipmentStatus: true,
		})
	if err != nil {
		log.Printf("error retrieving thermostat s for %s: %v", id, err)
		return
	}
	s := tsm[id]

	ts, err := e.GetThermostats(ecobee.Selection{
		SelectionType:  "thermostats",
		SelectionMatch: config.Ecobee.ThermostatId,
		IncludeRuntime: true,
		IncludeProgram: true,
		IncludeEvents:  true,
		IncludeSensors: true,
	})
	if err != nil {
		log.Printf("error retrieving thermostat: %v", err)
		return
	}

	t := ts[0]
	if s.RuntimeRevision == lastRevision {
		log.Println("--- No Update ---")
	} else {
		log.Printf("--- Got updated data on thermostat and %d sensors from Ecobee ---", len(t.RemoteSensors))
	}
	therm := ThermostatFields{
		CoolingSetpoint: float64(t.Runtime.DesiredCool) / 10,
		HeatingSetpoint: float64(t.Runtime.DesiredHeat) / 10,
		Program:         Program(t),
		EquipmentStatus: EquipmentStatus(s.EquipmentStatus),
		Heat1:           s.HeatPump,
		Heat2:           s.HeatPump2,
		Heat3:           s.HeatPump3,
		Cool1:           s.CompCool1,
		Cool2:           s.CompCool2,
		AuxHeat1:        s.AuxHeat1,
		AuxHeat2:        s.AuxHeat2,
		AuxHeat3:        s.AuxHeat3,
		Fan:             s.Fan,
		Idle: !s.HeatPump && !s.HeatPump2 && !s.HeatPump3 && !s.CompCool1 && !s.CompCool2 && !s.AuxHeat1 &&
			!s.AuxHeat2 && !s.AuxHeat3 && !s.Fan,
	}

	sensors := make([]Sensor, len(t.RemoteSensors)+1)
	for i, s := range t.RemoteSensors {
		var temp float64
		var occ bool
		var hum *float64
		var name string
		for _, c := range s.Capability {
			if c.Type == "temperature" {
				temp, err = strconv.ParseFloat(c.Value, 64)
				if err != nil {
					log.Printf("Unable to parse temp %s", c.Value)
				} else {
					temp = temp / 10
				}
			}
			if c.Type == "occupancy" {
				if c.Value == "true" {
					occ = true
				}
			}
			if c.Type == "humidity" {
				h, err := strconv.ParseFloat(c.Value, 64)
				if err != nil {
					log.Printf("Unable to parse humidity %s", c.Value)
				} else {
					hum = &h
				}
			}
			if strings.HasPrefix(s.ID, "rs:") {
				name = fmt.Sprintf("EcobeeSensor: %s (%s)", s.Name, s.Code)
			}
			if strings.HasPrefix(s.ID, "ei:") {
				name = fmt.Sprintf("EcobeeSensor: %s (Thermostat)", s.Name)
			}
		}
		sensors[i] = Sensor{
			Name: name,
			SensorFields: SensorFields{
				Temperature: temp,
				Occupancy:   occ,
				Humidity:    hum,
			},
		}
	}
	thermSensor := Sensor{
		Name: "EcobeeTherm: " + t.Name,
		SensorFields: SensorFields{
			Temperature: float64(t.Runtime.ActualTemperature) / 10,
			Occupancy:   AllOccupancy(sensors),
		},
	}
	sensors[len(sensors)-1] = thermSensor

	points := make([]*write.Point, 0)
	addPoint := func(metric interface{}, name string, measurement string) {
		point := FieldsToPoint(metric, now, name, measurement)
		points = append(points, point)
	}

	for _, sensor := range sensors {
		addPoint(sensor.SensorFields, sensor.Name, config.InfluxDB.Measurements.Sensor)
	}
	addPoint(therm, t.Name, config.InfluxDB.Measurements.Thermostat)
	err = writeApi.WritePoint(context.Background(), points...)
	if err != nil {
		log.Printf("Write failed: %+v", errors.WithStack(err))
	}
	lastRevision = s.RuntimeRevision
	return
}

func FieldsToPoint(value interface{}, now time.Time, name string, measurement string) *write.Point {
	tags := map[string]string{"name": name}
	fields := make(map[string]interface{})
	e := reflect.ValueOf(value)
	for i := 0; i < e.NumField(); i++ {
		name := strcase.ToSnake(e.Type().Field(i).Name)
		field := e.Field(i)
		if field.Kind() == reflect.Ptr && field.IsNil() {
			// don't set value for nil pointers
			continue
		}
		val := reflect.Indirect(field).Interface()
		fields[name] = val
	}
	return write.NewPoint(measurement, tags, fields, now)
}

func EquipmentStatus(status ecobee.EquipmentStatus) string {
	if status.HeatPump {
		return "Heat1"
	} else if status.HeatPump2 {
		return "Heat2"
	} else if status.HeatPump3 {
		return "Heat3"
	} else if status.CompCool1 {
		return "Cool1"
	} else if status.CompCool2 {
		return "Cool2"
	} else if status.AuxHeat1 {
		return "Aux1"
	} else if status.AuxHeat2 {
		return "Aux2"
	} else if status.AuxHeat3 {
		return "Aux3"
	} else if status.Fan {
		return "Fan"
	} else {
		return "Idle"
	}
}

func Program(t ecobee.Thermostat) string {
	for _, e := range t.Events {
		if e.Running {
			switch e.Type {
			case "vacation":
				return "vacation"
			case "hold":
				return "hold"
			}
		}
	}
	return t.Program.CurrentClimateRef
}

func AllOccupancy(sensors []Sensor) bool {
	for _, sensor := range sensors {
		if sensor.Occupancy {
			return true
		}
	}
	return false
}

type Config struct {
	InfluxDB struct {
		Host         string
		AuthToken    string `yaml:"auth_token"`
		Org          string
		Bucket       string
		Measurements struct {
			Thermostat string
			Sensor     string
		}
	}
	Ecobee struct {
		ThermostatId  string `yaml:"thermostat_id"`
		AppId         string `yaml:"app_id"`
		AuthCacheFile string `yaml:"auth_cache_file"`
		PollCron      string `yaml:"poll_cron"`
	}
}

type ThermostatFields struct {
	CoolingSetpoint float64
	HeatingSetpoint float64
	Program         string // we will include vacation here, since program doesn't matter during vacation mode
	EquipmentStatus string // Text version of bools for discrete graph
	Heat1           bool
	Heat2           bool
	Heat3           bool
	Cool1           bool
	Cool2           bool
	AuxHeat1        bool
	AuxHeat2        bool
	AuxHeat3        bool
	Fan             bool
	Idle            bool
}

type SensorFields struct {
	Temperature float64
	Occupancy   bool // note: may need string also for discrete graph?
	Humidity    *float64
}

type Sensor struct {
	Name string
	SensorFields
}
