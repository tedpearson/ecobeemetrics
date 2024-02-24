package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-co-op/gocron/v2"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
	"github.com/rspier/go-ecobee/ecobee"
	"gopkg.in/yaml.v3"
)

var (
	version   = "development"
	goVersion = "unknown"
	buildDate = "unknown"
)

func main() {
	configFile := flag.String("config", "ecobeemetrics.yaml", "Config file")
	getPin := flag.Bool("getpin", false, "Get ecobee pin only")
	saveToken := flag.String("savetoken", "", "Ecobee code to get auth token")
	versionFlag := flag.Bool("version", false, "Show version and exit")
	flag.Parse()
	fmt.Printf("ecobeemetrics version %s built on %s with %s\n", version, buildDate, goVersion)
	if *versionFlag {
		os.Exit(0)
	}

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
			log.Fatalf("Error authorizing with ecobee: %v", err)
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
		gocron.NewTask(run, e, write, config),
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
var lastUpdate = time.Now()
var apiFailures int

func run(e *ecobee.Client, writeApi api.WriteAPIBlocking, config Config) {
	now := time.Now()
	id := config.Ecobee.ThermostatId

	summaries, err := e.GetThermostatSummary(
		ecobee.Selection{
			SelectionType:          "thermostats",
			SelectionMatch:         id,
			IncludeEquipmentStatus: true,
		})
	if err != nil {
		log.Printf("error retrieving thermostat s for %s: %v", id, err)
		apiFailures++
		writeAppMetrics(writeApi, apiFailures, lastUpdate, now)
		return
	}
	summary := summaries[id]

	thermostats, err := e.GetThermostats(ecobee.Selection{
		SelectionType:  "thermostats",
		SelectionMatch: id,
		IncludeRuntime: true,
		IncludeEvents:  true,
		IncludeSensors: true,
	})
	if err != nil {
		log.Printf("error retrieving thermostat: %v", err)
		apiFailures++
		writeAppMetrics(writeApi, apiFailures, lastUpdate, now)
		return
	}
	thermostat := thermostats[0]

	if summary.RuntimeRevision != lastRevision {
		lastUpdate = now
		log.Printf("--- Got updated data on thermostat and %d sensors from Ecobee ---", len(thermostat.RemoteSensors))
	}

	writeAppMetrics(writeApi, apiFailures, lastUpdate, now)

	if summary.RuntimeRevision == lastRevision {
		if lastUpdate.Add(time.Minute * 15).Before(now) {
			// don't write any data if it's more than 15 minutes stale
			log.Println("--- No Update, Stale Data (not writing to database) ---")
			return
		}
		log.Println("--- No Update ---")
	}

	lastRevision = summary.RuntimeRevision

	thermostatPoint := write.NewPointWithMeasurement(config.InfluxDB.Measurements.Thermostat).
		SetTime(now).
		AddTag("name", thermostat.Name).
		AddField("cooling_setpoint", float64(thermostat.Runtime.DesiredCool)/10).
		AddField("heating_setpoint", float64(thermostat.Runtime.DesiredHeat)/10).
		AddField("heat1", summary.HeatPump).
		AddField("heat2", summary.HeatPump2).
		AddField("heat3", summary.HeatPump3).
		AddField("cool1", summary.CompCool1).
		AddField("cool2", summary.CompCool2).
		AddField("aux_heat1", summary.AuxHeat1).
		AddField("aux_heat2", summary.AuxHeat2).
		AddField("aux_heat3", summary.AuxHeat3).
		AddField("fan", summary.Fan).
		AddField("idle", !summary.HeatPump && !summary.HeatPump2 && !summary.HeatPump3 && !summary.CompCool1 &&
			!summary.CompCool2 && !summary.AuxHeat1 && !summary.AuxHeat2 && !summary.AuxHeat3 && !summary.Fan)
	err = writeApi.WritePoint(context.Background(), thermostatPoint)
	if err != nil {
		fmt.Printf("Write failed: %v\n", err)
	}

	points := make([]*write.Point, len(thermostat.RemoteSensors)+1)
	allOccupancy := false
	for i, s := range thermostat.RemoteSensors {
		point := write.NewPointWithMeasurement(config.InfluxDB.Measurements.Sensor).
			SetTime(now)
		points[i] = point
		for _, c := range s.Capability {
			if c.Type == "temperature" {
				temp, err := strconv.ParseFloat(c.Value, 64)
				if err != nil {
					fmt.Printf("Unable to parse temp %s\n", c.Value)
				} else {
					point.AddField("temperature", temp/10)
				}
			}
			if c.Type == "occupancy" {
				if c.Value == "true" {
					allOccupancy = true
					point.AddField("occupancy", true)
				}
			}
			if c.Type == "humidity" {
				hum, err := strconv.ParseFloat(c.Value, 64)
				if err != nil {
					fmt.Printf("Unable to parse humidity %s\n", c.Value)
				} else {
					point.AddField("humidity", hum)
				}
			}
			var name string
			if strings.HasPrefix(s.ID, "rs:") {
				name = fmt.Sprintf("EcobeeSensor: %s (%s)", s.Name, s.Code)
			}
			if strings.HasPrefix(s.ID, "ei:") {
				name = fmt.Sprintf("EcobeeSensor: %s (Thermostat)", s.Name)
			}
			point.AddTag("name", name)
		}
	}
	points[len(points)-1] = write.NewPointWithMeasurement(config.InfluxDB.Measurements.Sensor).
		SetTime(now).
		AddTag("name", "EcobeeTherm: "+thermostat.Name).
		AddField("temperature", float64(thermostat.Runtime.ActualTemperature)/10).
		AddField("occupancy", allOccupancy)
	err = writeApi.WritePoint(context.Background(), points...)
	if err != nil {
		fmt.Printf("Write failed: %v\n", err)
	}
}

func writeAppMetrics(writeApi api.WriteAPIBlocking, apiFailures int, lastUpdate time.Time, now time.Time) {
	point := write.NewPointWithMeasurement("ecobeemetrics").
		SetTime(now).
		AddField("api_failure_total", apiFailures).
		AddField("last_update_timestamp_seconds", lastUpdate.Unix())
	err := writeApi.WritePoint(context.Background(), point)
	if err != nil {
		fmt.Printf("Write failed: %v\n", err)
	}
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
