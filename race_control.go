package servermanager

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cj123/assetto-server-manager/pkg/udp"
	"github.com/google/uuid"
	"github.com/mitchellh/go-wordwrap"
	"github.com/sirupsen/logrus"
)

type RaceControl struct {
	process          ServerProcess
	store            Store
	penaltiesManager *PenaltiesManager

	SessionInfo      udp.SessionInfo `json:"SessionInfo"`
	TrackMapData     TrackMapData    `json:"TrackMapData"`
	TrackInfo        TrackInfo       `json:"TrackInfo"`
	SessionStartTime time.Time       `json:"SessionStartTime"`

	ConnectedDrivers    *DriverMap `json:"ConnectedDrivers"`
	DisconnectedDrivers *DriverMap `json:"DisconnectedDrivers"`

	CarIDToGUID      map[udp.CarID]udp.DriverGUID `json:"CarIDToGUID"`
	carIDToGUIDMutex sync.RWMutex

	sessionInfoTicker  *time.Ticker
	sessionInfoContext context.Context
	sessionInfoCfn     context.CancelFunc

	broadcaster                  Broadcaster
	trackDataGateway             TrackDataGateway
	driverGUIDUpdateCounter      map[udp.DriverGUID]int
	driverGUIDUpdateCounterMutex sync.RWMutex

	driverSwapTimers      map[int]*time.Timer
	persistStoreDataMutex sync.Mutex
}

// RaceControl piggyback's on the udp.Message interface so that the entire data can be sent to newly connected clients.
func (rc *RaceControl) Event() udp.Event {
	return 200
}

type CollisionType string

const (
	CollisionWithCar         CollisionType = "with other car"
	CollisionWithEnvironment CollisionType = "with environment"
)

type Collision struct {
	ID              string         `json:"ID"`
	Type            CollisionType  `json:"Type"`
	Time            time.Time      `json:"Time" ts:"date"`
	OtherDriverGUID udp.DriverGUID `json:"OtherDriverGUID"`
	OtherDriverName string         `json:"OtherDriverName"`
	Speed           float64        `json:"Speed"`
}

func NewRaceControl(broadcaster Broadcaster, trackDataGateway TrackDataGateway, process ServerProcess, store Store, penaltiesManager *PenaltiesManager) *RaceControl {
	rc := &RaceControl{
		broadcaster:      broadcaster,
		trackDataGateway: trackDataGateway,
		process:          process,
		driverSwapTimers: make(map[int]*time.Timer),
		store:            store,
		penaltiesManager: penaltiesManager,
	}

	rc.clearAllDrivers()

	return rc
}

func (rc *RaceControl) UDPCallback(message udp.Message) {
	var err error

	sendUpdatedRaceControlStatus := false

	switch m := message.(type) {
	case udp.Version:
		err = rc.OnVersion(m)
	case udp.SessionInfo:
		if m.Event() == udp.EventNewSession {
			err = rc.OnNewSession(m)
			sendUpdatedRaceControlStatus = true
		} else {
			sendUpdatedRaceControlStatus, err = rc.OnSessionUpdate(m)
		}

	case udp.EndSession:
		err = rc.OnEndSession(m)

		sendUpdatedRaceControlStatus = true
	case udp.CarUpdate:
		err = rc.OnCarUpdate(m)
	case udp.SessionCarInfo:
		if m.Event() == udp.EventNewConnection {
			err = rc.OnClientConnect(m)
		} else if m.Event() == udp.EventConnectionClosed {
			err = rc.OnClientDisconnect(m)
		}

		sendUpdatedRaceControlStatus = true
	case udp.ClientLoaded:
		err = rc.OnClientLoaded(m)

		sendUpdatedRaceControlStatus = true
	case udp.CollisionWithCar:
		err = rc.OnCollisionWithCar(m)
		sendUpdatedRaceControlStatus = true
	case udp.CollisionWithEnvironment:
		err = rc.OnCollisionWithEnvironment(m)
		sendUpdatedRaceControlStatus = true
	case udp.LapCompleted:
		err = rc.OnLapCompleted(m)

		sendUpdatedRaceControlStatus = true
	default:
		// unhandled event
		return
	}

	if err != nil {
		logrus.WithError(err).Errorf("Unable to handle event: %d", message.Event())
		return
	}

	if sendUpdatedRaceControlStatus {
		err = rc.broadcaster.Send(rc)

		if err != nil {
			logrus.WithError(err).Error("Unable to broadcast race control message")
			return
		}

		go rc.persistTimingData()
	}
}

// OnVersion occurs when the Assetto Corsa Server starts up for the first time.
func (rc *RaceControl) OnVersion(version udp.Version) error {
	return rc.broadcaster.Send(version)
}

// OnCarUpdate occurs every udp.RealTimePosInterval and returns car position, speed, etc.
// drivers top speeds are recorded per lap, as well as their last seen updated.
func (rc *RaceControl) OnCarUpdate(update udp.CarUpdate) error {
	driver, err := rc.findConnectedDriverByCarID(update.CarID)

	if err != nil {
		return err
	}

	var driversToDisconnect []*RaceControlDriver

	rc.driverGUIDUpdateCounterMutex.Lock()

	for guid := range rc.driverGUIDUpdateCounter {
		rc.driverGUIDUpdateCounter[guid]++

		// driver has missed 5 car updates, alt+f4/game crash?
		if rc.driverGUIDUpdateCounter[guid] > rc.ConnectedDrivers.Len()*5 {
			disconnectedDriver, ok := rc.ConnectedDrivers.Get(guid)

			if ok {
				driversToDisconnect = append(driversToDisconnect, disconnectedDriver)
			}
		}
	}

	rc.driverGUIDUpdateCounterMutex.Unlock()

	for _, driver := range driversToDisconnect {
		logrus.Debugf("Driver: %s (%s) has missed 5 car updates, disconnecting", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID)
		err := rc.disconnectDriver(driver)

		if err != nil {
			logrus.WithError(err).Errorf("Could not disconnect driver: %s (%s)", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID)
			continue
		}
	}

	// reset the counter for this car
	rc.driverGUIDUpdateCounterMutex.Lock()
	rc.driverGUIDUpdateCounter[driver.CarInfo.DriverGUID] = 0
	rc.driverGUIDUpdateCounterMutex.Unlock()

	speed := metersPerSecondToKilometersPerHour(
		math.Sqrt(math.Pow(float64(update.Velocity.X), 2) + math.Pow(float64(update.Velocity.Z), 2)),
	)

	if speed > driver.CurrentCar().TopSpeedThisLap {
		driver.CurrentCar().TopSpeedThisLap = speed
	}

	driver.LastSeen = time.Now()
	driver.LastPos = update.Pos

	if len(driversToDisconnect) > 0 {
		err := rc.broadcaster.Send(rc)

		if err != nil {
			return err
		}
	}

	return rc.broadcaster.Send(update)
}

// OnNewSession occurs every new session. If the session is the first in an event and it is not a looped practice,
// then all driver information is cleared.
func (rc *RaceControl) OnNewSession(sessionInfo udp.SessionInfo) error {
	oldSessionInfo := rc.SessionInfo
	rc.SessionInfo = sessionInfo
	rc.SessionStartTime = time.Now()

	rc.driverGUIDUpdateCounterMutex.Lock()
	rc.driverGUIDUpdateCounter = make(map[udp.DriverGUID]int)
	rc.driverGUIDUpdateCounterMutex.Unlock()

	emptyCarInfo := true

	driverSwapPenalties = make(map[string]*driverPenalty)

	if (rc.ConnectedDrivers.Len() > 0 || rc.DisconnectedDrivers.Len() > 0) && sessionInfo.Type == udp.SessionTypePractice {
		if oldSessionInfo.Type == sessionInfo.Type && oldSessionInfo.Track == sessionInfo.Track && oldSessionInfo.TrackConfig == sessionInfo.TrackConfig && oldSessionInfo.Name == sessionInfo.Name {
			// this is a looped event, keep the cars
			emptyCarInfo = false
		}
	}

	if emptyCarInfo {
		_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
			*driver = *NewRaceControlDriver(driver.CarInfo)

			return nil
		})

		// all disconnected drivers are removed when car info is emptied, otherwise we are just showing empty entries in
		// the disconnected drivers table, which is pointless.
		rc.DisconnectedDrivers = NewDriverMap(DisconnectedDrivers, rc.SortDrivers)
	}

	// clear out last lap completed time each new session
	_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
		driver.CurrentCar().LastLapCompletedTime = time.Now()

		return nil
	})

	var err error

	trackInfo, err := rc.trackDataGateway.TrackInfo(sessionInfo.Track, sessionInfo.TrackConfig)

	if err != nil {
		return err
	}

	rc.TrackInfo = *trackInfo

	trackMapData, err := rc.trackDataGateway.TrackMap(sessionInfo.Track, sessionInfo.TrackConfig)

	if err != nil {
		logrus.WithError(err).Errorf("Could not load track map data")
	} else {
		rc.TrackMapData = *trackMapData
	}

	logrus.Debugf("New session detected: %s at %s (%s) [emptyCarInfo: %t]", sessionInfo.Type.String(), sessionInfo.Track, sessionInfo.TrackConfig, emptyCarInfo)

	go rc.requestSessionInfo()

	// look for live timings stored previously
	persistedInfo, err := rc.store.LoadLiveTimingsData()

	if err == nil && persistedInfo != nil {
		if persistedInfo.SessionType == rc.SessionInfo.Type &&
			persistedInfo.Track == rc.SessionInfo.Track &&
			persistedInfo.TrackLayout == rc.SessionInfo.TrackConfig &&
			persistedInfo.SessionName == rc.SessionInfo.Name {

			for guid, driver := range persistedInfo.Drivers {
				_, driverPresentInDisconnectedList := rc.DisconnectedDrivers.Get(guid)
				_, driverPresentInConnectedList := rc.ConnectedDrivers.Get(guid)

				if !driverPresentInConnectedList && !driverPresentInDisconnectedList {
					rc.DisconnectedDrivers.Add(guid, driver)
				}
			}

			logrus.Infof("Loaded previous Live Timings data for %s (%s), num drivers: %d", persistedInfo.Track, persistedInfo.TrackLayout, len(persistedInfo.Drivers))
		}
	} else {
		logrus.WithError(err).Debugf("Could not load persisted live timings practice data")
	}

	return rc.broadcaster.Send(sessionInfo)
}

// clearAllDrivers removes all known information about connected and disconnected drivers from RaceControl
func (rc *RaceControl) clearAllDrivers() {
	rc.ConnectedDrivers = NewDriverMap(ConnectedDrivers, rc.SortDrivers)
	rc.DisconnectedDrivers = NewDriverMap(DisconnectedDrivers, rc.SortDrivers)
	rc.carIDToGUIDMutex.Lock()
	rc.CarIDToGUID = make(map[udp.CarID]udp.DriverGUID)
	rc.carIDToGUIDMutex.Unlock()
	rc.driverGUIDUpdateCounterMutex.Lock()
	rc.driverGUIDUpdateCounter = make(map[udp.DriverGUID]int)
	rc.driverGUIDUpdateCounterMutex.Unlock()
}

var sessionInfoRequestInterval = time.Second * 30

// requestSessionInfo sends a request every sessionInfoRequestInterval to get information about temps, etc in the session.
func (rc *RaceControl) requestSessionInfo() {
	if rc.sessionInfoTicker != nil {
		rc.sessionInfoTicker.Stop()
	}

	rc.sessionInfoTicker = time.NewTicker(sessionInfoRequestInterval)
	rc.sessionInfoContext, rc.sessionInfoCfn = context.WithCancel(context.Background())

	for {
		select {
		case <-rc.sessionInfoTicker.C:
			err := rc.process.SendUDPMessage(udp.GetSessionInfo{})

			if err == ErrNoOpenUDPConnection {
				logrus.WithError(err).Warnf("Couldn't send session info udp request. Breaking loop.")
				rc.sessionInfoTicker.Stop()
				return
			} else if err != nil {
				logrus.WithError(err).Errorf("Couldn't send session info udp request")
			}

		case <-rc.process.Done():
			rc.sessionInfoTicker.Stop()

			logrus.Debugf("Assetto Process completed. Disconnecting all connected drivers. Session done.")

			var drivers []*RaceControlDriver

			// the server has just stopped. send disconnect messages for all connected cars.
			_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
				// Each takes a read lock, so we cannot call disconnectDriver (which takes a write lock) from inside it.
				// we must instead append them to a slice and disconnect them outside the Each call.
				drivers = append(drivers, driver)

				return nil
			})

			for _, driver := range drivers {
				// disconnect the driver
				err := rc.disconnectDriver(driver)

				if err != nil {
					logrus.WithError(err).Errorf("Could not disconnect driver: %s (%s)", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID)
					continue
				}
			}

			if err := rc.broadcaster.Send(rc); err != nil {
				logrus.WithError(err).Errorf("Couldn't broadcast race control")
			}

		case <-rc.sessionInfoContext.Done():
			rc.sessionInfoTicker.Stop()
			return
		}
	}
}

func (rc *RaceControl) disconnectDriver(driver *RaceControlDriver) error {
	carInfo := driver.CarInfo
	carInfo.EventType = udp.EventConnectionClosed
	return rc.OnClientDisconnect(carInfo)
}

// OnSessionUpdate is called every sessionRequestInterval.
func (rc *RaceControl) OnSessionUpdate(sessionInfo udp.SessionInfo) (bool, error) {
	oldSessionInfo := rc.SessionInfo

	// we can't just copy over the session information, we must copy individual
	// parts of it, as the session type is incorrect.
	rc.SessionInfo.AmbientTemp = sessionInfo.AmbientTemp
	rc.SessionInfo.RoadTemp = sessionInfo.RoadTemp
	rc.SessionInfo.WeatherGraphics = sessionInfo.WeatherGraphics
	rc.SessionInfo.ElapsedMilliseconds = sessionInfo.ElapsedMilliseconds

	sessionHasChanged := oldSessionInfo.AmbientTemp != rc.SessionInfo.AmbientTemp || oldSessionInfo.RoadTemp != rc.SessionInfo.RoadTemp || oldSessionInfo.WeatherGraphics != rc.SessionInfo.WeatherGraphics

	return sessionHasChanged, nil
}

// OnEndSession is called at the end of every session.
func (rc *RaceControl) OnEndSession(sessionFile udp.EndSession) error {
	if rc.sessionInfoCfn != nil {
		rc.sessionInfoCfn()
	}

	for _, driver := range rc.ConnectedDrivers.Drivers {
		if driver.driverSwapCfn != nil {
			driver.driverSwapCfn()
		}
	}

	filename := filepath.Base(string(sessionFile))
	logrus.Infof("End Session, file outputted at: %s", filename)

	// loop over driverSwapPenalties and apply penalty
	for guid, penalty := range driverSwapPenalties {
		err := rc.penaltiesManager.applyPenalty(filename, guid, penalty.carModel, penalty.penalty.Seconds(), true)

		if err != nil {
			logrus.WithError(err).Errorf("could not apply driver swap penalty of %s to driver %s", penalty.penalty.String(), guid)
			continue
		}
	}

	return nil
}

// OnClientConnect stores CarID -> DriverGUID mappings. if a driver is known to have previously been in this event,
// they will be moved from DisconnectedDrivers to ConnectedDrivers.
func (rc *RaceControl) OnClientConnect(client udp.SessionCarInfo) error {
	rc.carIDToGUIDMutex.Lock()
	rc.CarIDToGUID[client.CarID] = client.DriverGUID
	rc.carIDToGUIDMutex.Unlock()

	client.DriverInitials = driverInitials(client.DriverName)
	client.DriverName = driverName(client.DriverName)

	var driver *RaceControlDriver

	if disconnectedDriver, ok := rc.DisconnectedDrivers.Get(client.DriverGUID); ok {
		driver = disconnectedDriver
		driver.CarInfo = client
		logrus.Debugf("Driver %s (%s) reconnected in %s (car id: %d)", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID, driver.CarInfo.CarModel, client.CarID)
		rc.DisconnectedDrivers.Del(client.DriverGUID)
	} else {
		driver = NewRaceControlDriver(client)
		logrus.Debugf("Driver %s (%s) connected in %s (car id: %d)", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID, driver.CarInfo.CarModel, client.CarID)
	}

	if _, ok := driver.Cars[driver.CarInfo.CarModel]; !ok {
		driver.Cars[driver.CarInfo.CarModel] = &RaceControlCarLapInfo{}
	}

	driver.ConnectedTime = time.Now()
	driver.CurrentCar().LastLapCompletedTime = time.Now()

	rc.ConnectedDrivers.Add(driver.CarInfo.DriverGUID, driver)

	return rc.broadcaster.Send(client)
}

// OnClientDisconnect moves a client from ConnectedDrivers to DisconnectedDrivers.
func (rc *RaceControl) OnClientDisconnect(client udp.SessionCarInfo) error {
	rc.driverGUIDUpdateCounterMutex.Lock()
	delete(rc.driverGUIDUpdateCounter, client.DriverGUID)
	rc.driverGUIDUpdateCounterMutex.Unlock()

	driver, ok := rc.ConnectedDrivers.Get(client.DriverGUID)

	if !ok {
		return fmt.Errorf("racecontrol: client disconnected without ever being connected: %s (%s)", client.DriverName, client.DriverGUID)
	}

	logrus.Debugf("Driver %s (%s) disconnected", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID)

	driver.LoadedTime = time.Time{}

	rc.ConnectedDrivers.Del(driver.CarInfo.DriverGUID)

	if driver.TotalNumLaps > 0 {
		rc.DisconnectedDrivers.Add(driver.CarInfo.DriverGUID, driver)
	}

	config := rc.process.GetServerConfig()

	// if this race has driver swaps enabled we should initialise one now
	if config.CurrentRaceConfig.DriverSwapEnabled == 1 /*&& rc.SessionInfo.Type.String() == SessionTypeRace.String()*/ {
		ticker := time.NewTicker(time.Second)

		go func() {
			rc.handleDriverSwap(ticker, config, client, driver)
		}()
	}

	return rc.broadcaster.Send(client)
}

type driverPenalty struct {
	penalty  time.Duration
	carModel string
}

var driverSwapPenalties map[string]*driverPenalty // map of GUID to penalty time

func (rc *RaceControl) handleDriverSwap(ticker *time.Ticker, config ServerConfig, client udp.SessionCarInfo, driver *RaceControlDriver) {
	var totalTime time.Duration
	var position udp.Vec
	var newDriverConnected bool
	var firstPositionUpdate bool

	completeTime := time.Second * time.Duration(config.CurrentRaceConfig.DriverSwapMinTime)
	initialGUID := client.DriverGUID
	currentDriver := driver

	position = currentDriver.LastPos

	logrus.Infof("Driver: %d has initiated a driver swap, disconnected in position: %.2f, %.2f, %.2f. Next driver is expected to connect in the same position for a driver swap!",
		currentDriver.CarInfo.CarID, currentDriver.LastPos.X, currentDriver.LastPos.Y, currentDriver.LastPos.Z)

	driver.driverSwapContext, driver.driverSwapCfn = context.WithCancel(context.Background())

	for {
		select {
		case <-driver.driverSwapContext.Done():
			return
		case <-ticker.C:
			totalTime += time.Second

			countdown := completeTime - totalTime

			if !newDriverConnected {
				reconnect := false

				_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
					if driver.CarInfo.CarID == currentDriver.CarInfo.CarID {
						if driver.CarInfo.DriverGUID != currentDriver.CarInfo.DriverGUID {
							if !driver.LoadedTime.IsZero() {
								// new driver has connected in the same car
								currentDriver = driver

								newDriverConnected = true

								logrus.Infof("Driver: %d (%s) has connected", currentDriver.CarInfo.CarID, currentDriver.CarInfo.DriverGUID)
							}
						} else {
							// same driver reconnected
							logrus.Infof("Driver: %s has reconnected, driver swap aborted", initialGUID)
							reconnect = true
						}
					}

					return nil
				})

				if reconnect {
					ticker.Stop()
					return
				}
			} else {
				if totalTime.Seconds() >= completeTime.Seconds() {
					sendChat, err := udp.NewSendChat(currentDriver.CarInfo.CarID,
						fmt.Sprintf("You are clear to leave the pits, go go go!"))

					if err == nil {
						err := rc.process.SendUDPMessage(sendChat)

						if err != nil {
							logrus.WithError(err).Errorf("Unable to send driver swap clear to leave message to: %s", currentDriver.CarInfo.DriverName)
						}
					} else {
						logrus.WithError(err).Errorf("Unable to build driver swap clear to leave message to: %s", currentDriver.CarInfo.DriverName)
					}

					logrus.Infof("Driver: %d has successfully completed their driver swap and is free to leave the pits", currentDriver.CarInfo.CarID)

					ticker.Stop()
					return
				} else {

					if !firstPositionUpdate {
						var nilVec udp.Vec

						nilVec = udp.Vec{X: 0, Y: 0, Z: 0}

						if currentDriver.LastPos != nilVec {
							sendChat, err := udp.NewSendChat(currentDriver.CarInfo.CarID,
								fmt.Sprintf("Hi! You are mid way through a driver swap, please wait %s before leaving the pits", countdown.String()))

							if err == nil {
								err := rc.process.SendUDPMessage(sendChat)

								if err != nil {
									logrus.WithError(err).Errorf("Unable to send driver swap welcome message to: %s", currentDriver.CarInfo.DriverName)
								}
							} else {
								logrus.WithError(err).Errorf("Unable to build driver swap welcome message to: %s", currentDriver.CarInfo.DriverName)
							}

							firstPositionUpdate = true
						}
					}

					// if driver has moved
					if rc.positionHasChanged(position, currentDriver.LastPos) && firstPositionUpdate {
						// if the time is within the disqualify window
						if countdown >= (time.Second * time.Duration(config.CurrentRaceConfig.DriverSwapDisqualifyTime)) {
							sendChat, err := udp.NewSendChat(currentDriver.CarInfo.CarID,
								fmt.Sprintf("You have been kicked from the session for leaving the pits %s early during a driver swap", countdown.String()))

							if err == nil {
								err := rc.process.SendUDPMessage(sendChat)

								if err != nil {
									logrus.WithError(err).Errorf("Unable to send driver swap kicked message to: %s", currentDriver.CarInfo.DriverName)
								}
							} else {
								logrus.WithError(err).Errorf("Unable to build driver swap kicked message to: %s", currentDriver.CarInfo.DriverName)
							}

							time.Sleep(5 * time.Second)

							kickUser := udp.NewKickUser(uint8(currentDriver.CarInfo.CarID))

							err = rc.process.SendUDPMessage(kickUser)

							if err != nil {
								logrus.WithError(err).Errorf("Unable to send kick command (driver swaps)")
							} else {
								logrus.Infof("Driver: %d has been kicked for leaving the pits %s early during a driver swap", currentDriver.CarInfo.CarID, countdown.String())
							}

							// don't stop the ticker, when the driver reconnects they should still have to wait
						} else if countdown >= (time.Second * time.Duration(config.CurrentRaceConfig.DriverSwapPenaltyTime)) {

							if _, ok := driverSwapPenalties[string(currentDriver.CarInfo.DriverGUID)]; ok {
								driverSwapPenalties[string(currentDriver.CarInfo.DriverGUID)].penalty += countdown + (time.Second * 5)
							} else {
								driverSwapPenalties[string(currentDriver.CarInfo.DriverGUID)] = &driverPenalty{
									penalty:  countdown + (time.Second * 5),
									carModel: currentDriver.CarInfo.CarModel,
								}
							}

							sendChat, err := udp.NewSendChat(currentDriver.CarInfo.CarID,
								fmt.Sprintf("You have been given a %s second penalty for leaving the pits %s early during a driver swap", (countdown+(time.Second*5)).String(), countdown.String()))

							if err == nil {
								err := rc.process.SendUDPMessage(sendChat)

								if err != nil {
									logrus.WithError(err).Errorf("Unable to send driver swap penalty message to: %s", currentDriver.CarInfo.DriverName)
								}
							} else {
								logrus.WithError(err).Errorf("Unable to build driver swap penalty message to: %s", currentDriver.CarInfo.DriverName)
							}

							logrus.Infof("Driver: %d has been given a %s second penalty for leaving the pits %s early during a driver swap", currentDriver.CarInfo.CarID, (countdown + (time.Second * 5)).String(), countdown.String())

							ticker.Stop()
							return
						}

					}

					// send countdown messages
					if firstPositionUpdate {
						sendChat, err := udp.NewSendChat(currentDriver.CarInfo.CarID,
							fmt.Sprintf("Free to leave pits in %s", countdown.String()))

						if err == nil {
							err := rc.process.SendUDPMessage(sendChat)

							if err != nil {
								logrus.WithError(err).Errorf("Unable to send driver swap countdown message to: %s", currentDriver.CarInfo.DriverName)
							}
						} else {
							logrus.WithError(err).Errorf("Unable to build driver swap countdown message to: %s", currentDriver.CarInfo.DriverName)
						}
					}
				}
			}
		}
	}
}

func (rc *RaceControl) positionHasChanged(initialPosition, currentPosition udp.Vec) bool {
	fmt.Println(fmt.Sprintf("initial position: %.2f, %.2f, %.2f", initialPosition.X, initialPosition.Y, initialPosition.Z))
	fmt.Println(fmt.Sprintf("current position: %.2f, %.2f, %.2f", currentPosition.X, currentPosition.Y, currentPosition.Z))

	return math.Abs(float64(initialPosition.X-currentPosition.X)) >= 10.0 ||
		math.Abs(float64(initialPosition.Y-currentPosition.Y)) >= 10.0 ||
		math.Abs(float64(initialPosition.Z-currentPosition.Z)) >= 10.0
}

// findConnectedDriverByCarID looks for a driver in ConnectedDrivers by their CarID. This is the only place CarID
// is used for a look-up, and it uses the CarIDToGUID map to perform the lookup.
func (rc *RaceControl) findConnectedDriverByCarID(carID udp.CarID) (*RaceControlDriver, error) {
	rc.carIDToGUIDMutex.RLock()
	driverGUID, ok := rc.CarIDToGUID[carID]
	rc.carIDToGUIDMutex.RUnlock()

	if !ok {
		return nil, fmt.Errorf("racecontrol: could not find DriverGUID for CarID: %d", carID)
	}

	driver, ok := rc.ConnectedDrivers.Get(driverGUID)

	if !ok {
		return nil, fmt.Errorf("racecontrol: could not find connected driver for DriverGUID: %s", driverGUID)
	}

	return driver, nil
}

// OnClientLoaded marks a connected client as having loaded in.
func (rc *RaceControl) OnClientLoaded(loadedCar udp.ClientLoaded) error {
	driver, err := rc.findConnectedDriverByCarID(udp.CarID(loadedCar))

	if err != nil {
		return err
	}

	serverConfig := rc.process.GetServerConfig()

	solWarning := ""
	liveLink := ""

	if serverConfig.CurrentRaceConfig.IsSol == 1 {
		solWarning = "This server is running Sol. For the best experience please install Sol, and remember the other drivers may be driving in night conditions."
	}

	if config != nil && config.HTTP.BaseURL != "" {
		liveLink = fmt.Sprintf("You can view live timings for this event at %s", config.HTTP.BaseURL+"/live-timing")
	}

	wrapped := strings.Split(wordwrap.WrapString(
		fmt.Sprintf(
			"Hi, %s! Welcome to the %s server! %s %s Make this race count! %s\n",
			driver.CarInfo.DriverName,
			serverConfig.GlobalServerConfig.GetName(),
			serverConfig.GlobalServerConfig.ServerJoinMessage,
			solWarning,
			liveLink,
		),
		60,
	), "\n")

	for _, msg := range wrapped {
		welcomeMessage, err := udp.NewSendChat(driver.CarInfo.CarID, msg)

		if err == nil {
			err := rc.process.SendUDPMessage(welcomeMessage)

			if err != nil {
				logrus.WithError(err).Errorf("Unable to send welcome message to: %s", driver.CarInfo.DriverName)
			}
		} else {
			logrus.WithError(err).Errorf("Unable to build welcome message to: %s", driver.CarInfo.DriverName)
		}
	}

	logrus.Debugf("Driver: %s (%s) loaded", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID)

	driver.LoadedTime = time.Now()

	return rc.broadcaster.Send(loadedCar)
}

// OnLapCompleted occurs every time a driver crosses the line. Lap information is collected for the driver
// and best lap time and top speed are calculated. OnLapCompleted also remembers the car the lap was completed in
// a PreviousCars map on the driver. This is so that lap times between different cars can be compared.
func (rc *RaceControl) OnLapCompleted(lap udp.LapCompleted) error {
	driver, err := rc.findConnectedDriverByCarID(lap.CarID)

	if err != nil {
		return err
	}

	lapDuration := lapToDuration(int(lap.LapTime))

	logrus.Debugf("Lap completed by driver: %s (%s), %s", driver.CarInfo.DriverName, driver.CarInfo.DriverGUID, lapDuration)

	driver.TotalNumLaps++
	currentCar := driver.CurrentCar()

	currentCar.TotalLapTime += lapDuration
	currentCar.LastLap = lapDuration
	currentCar.NumLaps++
	currentCar.LastLapCompletedTime = time.Now()

	if lap.Cuts == 0 && (lapDuration < currentCar.BestLap || currentCar.BestLap == 0) {
		currentCar.BestLap = lapDuration
		currentCar.TopSpeedBestLap = currentCar.TopSpeedThisLap
	}

	currentCar.TopSpeedThisLap = 0

	rc.ConnectedDrivers.sort()

	if rc.SessionInfo.Type == udp.SessionTypeRace {
		// calculate split
		if driver.Position == 1 {
			driver.Split = time.Duration(0).String()
		} else {
			_ = rc.ConnectedDrivers.Each(func(otherDriverGUID udp.DriverGUID, otherDriver *RaceControlDriver) error {
				if otherDriver.Position == driver.Position-1 {
					driverCar := driver.CurrentCar()
					otherDriverCar := otherDriver.CurrentCar()

					lapDifference := otherDriverCar.NumLaps - driverCar.NumLaps

					if lapDifference <= 0 {
						driver.Split = (driverCar.TotalLapTime - otherDriverCar.TotalLapTime).Round(time.Millisecond).String()
					} else if lapDifference == 1 {
						driver.Split = "1 lap"
					} else {
						driver.Split = fmt.Sprintf("%d laps", lapDifference)
					}
				}

				return nil
			})
		}
	} else {
		var previousCar *RaceControlCarLapInfo

		// gaps are calculated vs best lap
		_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
			if previousCar == nil {
				driver.Split = "0s"
			} else {
				car := driver.CurrentCar()

				if car.BestLap >= previousCar.BestLap && car.BestLap != 0 {
					driver.Split = (car.BestLap - previousCar.BestLap).String()
				} else {
					driver.Split = ""
				}
			}

			previousCar = driver.CurrentCar()

			return nil
		})
	}

	return nil
}

func (rc *RaceControl) SortDrivers(driverGroup RaceControlDriverGroup, driverA, driverB *RaceControlDriver) bool {
	driverACar := driverA.CurrentCar()
	driverBCar := driverB.CurrentCar()

	if rc.SessionInfo.Type == udp.SessionTypeRace {
		if driverGroup == ConnectedDrivers {
			if driverACar.NumLaps == driverBCar.NumLaps {
				return driverACar.TotalLapTime < driverBCar.TotalLapTime
			} else {
				return driverACar.NumLaps > driverBCar.NumLaps
			}
		} else if driverGroup == DisconnectedDrivers {
			return driverACar.LastLapCompletedTime.After(driverBCar.LastLapCompletedTime)
		} else {
			panic("unknown driver group")
		}
	} else {
		if driverACar.BestLap == 0 && driverBCar.BestLap == 0 {
			if driverACar.NumLaps == driverBCar.NumLaps {
				return driverACar.LastLapCompletedTime.Before(driverBCar.LastLapCompletedTime)
			} else {
				return driverACar.NumLaps > driverBCar.NumLaps
			}
		}

		if driverACar.BestLap == 0 {
			return false
		} else if driverBCar.BestLap == 0 {
			return true
		}

		return driverACar.BestLap < driverBCar.BestLap
	}
}

func metersPerSecondToKilometersPerHour(mps float64) float64 {
	return mps * 3.6
}

// OnCollisionWithCar registers a driver's collision with another car.
func (rc *RaceControl) OnCollisionWithCar(collision udp.CollisionWithCar) error {
	driver, err := rc.findConnectedDriverByCarID(collision.CarID)

	if err != nil {
		return err
	}

	c := Collision{
		ID:    uuid.New().String(),
		Type:  CollisionWithCar,
		Time:  time.Now(),
		Speed: metersPerSecondToKilometersPerHour(float64(collision.ImpactSpeed)),
	}

	otherDriver, err := rc.findConnectedDriverByCarID(collision.OtherCarID)

	if err == nil {
		c.OtherDriverGUID = otherDriver.CarInfo.DriverGUID
		c.OtherDriverName = otherDriver.CarInfo.DriverName
	}

	driver.Collisions = append(driver.Collisions, c)

	return rc.broadcaster.Send(collision)
}

// OnCollisionWithEnvironment registers a driver's collision with the environment.
func (rc *RaceControl) OnCollisionWithEnvironment(collision udp.CollisionWithEnvironment) error {
	driver, err := rc.findConnectedDriverByCarID(collision.CarID)

	if err != nil {
		return err
	}

	driver.Collisions = append(driver.Collisions, Collision{
		ID:    uuid.New().String(),
		Type:  CollisionWithEnvironment,
		Time:  time.Now(),
		Speed: metersPerSecondToKilometersPerHour(float64(collision.ImpactSpeed)),
	})

	return rc.broadcaster.Send(collision)
}

type LiveTimingsPersistedData struct {
	SessionType udp.SessionType
	Track       string
	TrackLayout string
	SessionName string

	Drivers map[udp.DriverGUID]*RaceControlDriver
}

func (rc *RaceControl) persistTimingData() {
	rc.persistStoreDataMutex.Lock()
	defer rc.persistStoreDataMutex.Unlock()

	data := &LiveTimingsPersistedData{
		SessionType: rc.SessionInfo.Type,
		Track:       rc.SessionInfo.Track,
		TrackLayout: rc.SessionInfo.TrackConfig,
		SessionName: rc.SessionInfo.Name,

		Drivers: rc.AllLapTimes(),
	}

	err := rc.store.UpsertLiveTimingsData(data)

	if err != nil {
		logrus.WithError(err).Errorf("Could not save live timings data")
	}
}

func (rc *RaceControl) AllLapTimes() map[udp.DriverGUID]*RaceControlDriver {
	out := make(map[udp.DriverGUID]*RaceControlDriver)

	_ = rc.DisconnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
		out[driverGUID] = driver

		return nil
	})

	_ = rc.ConnectedDrivers.Each(func(driverGUID udp.DriverGUID, driver *RaceControlDriver) error {
		out[driverGUID] = driver

		return nil
	})

	return out
}

func lapToDuration(i int) time.Duration {
	d, _ := time.ParseDuration(fmt.Sprintf("%dms", i))

	return d
}
