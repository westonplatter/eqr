/*
# The MIT License (MIT)
#
# Copyright (c) 2019  Carbon Black
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.
*/
package ruleset

import (
	"container/list"
	"encoding/json"
	"errors"
	"reflect"
	"time"
	"fmt"

	"github.com/carbonblack/eqr/metrics"
	"github.com/sirupsen/logrus"

	chk "github.com/carbonblack/eqr/checkpoint"
	"github.com/carbonblack/eqr/logging"
	plg "github.com/carbonblack/eqr/ruleset/pluginInterfaces"
)

// This class is responsible for actually building the rules into those predicates/projections
// and their associated steps. Takes into account nested functions

type Dest struct {
	Plugin  *plg.IOPluginInterface
	Pointer interface{}
}
type Base struct {
	Line *list.List
}

type ResultInterface interface{}
type Step struct {
	ID        *string
	Plugin    *plg.PluginInterface
	Value     *string
	MultiArgs *list.List
	Result    ResultInterface
	GetPrev   bool
}

type Rulebase struct {
	RuleName     string
	MetricSender *metrics.MetricSender
	Predicate    *list.List
	Projection   *list.List
	Cache        *list.List
	Destination  *Dest
	Checkpoint   bool
}

var logger = logging.GetLogger()

func (r *Rulebase) RunRecordGeneration(fatalErr chan error) (err error) {
	// run the Consume
	logger.WithFields(logrus.Fields{
		"rule": (*r.Destination.Plugin).Name(),
	}).Debug("Running Consume")
	err = (*r.Destination.Plugin).Consume(r.Destination.Pointer, fatalErr)

	return err
}

// This runs a rule give the record
func (r *Rulebase) RunRule(recordStruct *chk.CheckpointIdentifier) {
	dimensions := make(map[string]string)
	dimensions["shardId"] = *recordStruct.ShardId
	dimensions["rule"] = r.RuleName

	startTime := time.Now()

	logger.WithFields(logrus.Fields{
		"rule":           (*r.Destination.Plugin).Name(),
		"sequenceNumber": recordStruct.Id,
	}).Debug("Running Predicate")

	res, _, err := r.runProjection(recordStruct.Payload, "PREDICATE")

	logger.WithFields(logrus.Fields{
		"rule":      (*r.Destination.Plugin).Name(),
		"predicate": res,
	}).Debug("Predicate result")

	if err != nil {
		logger.WithFields(logrus.Fields{
			"rule": (*r.Destination.Plugin).Name(),
			"err":  err.Error(),
		}).Fatal("Error happened in running predicate")
		return
	} else if res == false {
		if r.Checkpoint {
			recordStruct.Channel <- 1
		}
		return
	}

	// Do the cache
	logger.WithFields(logrus.Fields{
		"rule": (*r.Destination.Plugin).Name(),
	}).Debug("Starting cache")

	res, _, err = r.runProjection(recordStruct.Payload, "CACHE")

	if err != nil {
		logger.WithFields(logrus.Fields{
			"rule": (*r.Destination.Plugin).Name(),
			"err":  err.Error(),
		}).Fatal("Error happened in running cache")
		return
	} else if res == false {
		if r.Checkpoint {
			recordStruct.Channel <- 1
		}
		return
	}

	// do the projection
	logger.WithFields(logrus.Fields{
		"rule": (*r.Destination.Plugin).Name(),
	}).Debug("Starting projection")

	res, bytes, err := r.runProjection(recordStruct.Payload, "PROJECTION")

	logger.WithFields(logrus.Fields{
		"rule":   (*r.Destination.Plugin).Name(),
		"result": res,
	}).Debug("Finished projection")
	if err != nil {
		logger.WithFields(logrus.Fields{
			"rule": (*r.Destination.Plugin).Name(),
			"err":  err.Error(),
		}).Fatal("Error happened in running Projection")
		return
	} else if res == false || bytes == nil {
		if r.Checkpoint {
			recordStruct.Channel <- 1
		}
		return
	}

	(*r.MetricSender).SendCounter("rule_count", int64(1), dimensions)
	//r.MetricSender.SendCounter("rule_count", int64(1), dimensions)

	// downstream send!
	var retry int32
	for retry = 0; retry < 3; retry++ {
		logger.WithFields(logrus.Fields{
			"rule": (*r.Destination.Plugin).Name(),
		}).Debug("Starting publish")
		success, err := (*r.Destination.Plugin).Publish(r.Destination.Pointer, &bytes, dimensions)

		if success {
			if r.Checkpoint {
				recordStruct.Channel <- 1
			}
			logger.WithFields(logrus.Fields{
				"rule": (*r.Destination.Plugin).Name(),
			}).Debug("Successful publish")
			break
		} else {
			if r.Checkpoint && retry >= 2 {
				logger.WithFields(logrus.Fields{
					"rule": (*r.Destination.Plugin).Name(),
					"err":  err.Error(),
				}).Fatal("Unsuccessful publish")
				(*r.MetricSender).SendCounter("critical_errors_count", int64(1), dimensions)
			}
			logger.WithFields(logrus.Fields{
				"rule": (*r.Destination.Plugin).Name(),
				"err":  err.Error(),
			}).Error("Unsuccessful publish")
		}
	}

	endTime := time.Now()

	diff := endTime.Sub(startTime)
	execTime := float64(diff) / float64(time.Second)
	dimensions["retry_count"] = string(retry)
	(*r.MetricSender).SendGauge("rule_execution_time", execTime, dimensions)

	logger.WithFields(logrus.Fields{
		"rule": (*r.Destination.Plugin).Name(),
	}).Debug("Rule successfully run")

	return
}

// does all the projects of a record that will be further processed
// gets a JSON formatted byte array and passes it back
func (r *Rulebase) runProjection(record *string, ruleProperty string) (result bool, outbound []byte, err error) {

	downstream := make(map[string]interface{})
	var sendAllFlag bool
	var itr *list.Element
	if ruleProperty == "CACHE" {
		if r.Cache == nil {
			return true, nil, nil
		}
		itr = r.Cache.Front()
	} else if ruleProperty == "PROJECTION" {
		itr = r.Projection.Front()
	} else if ruleProperty == "PREDICATE" {
		itr = r.Predicate.Front()
	}
	for i := itr; i != nil; i = i.Next() {

		for j := i.Value.(Base).Line.Front(); j != nil; j = j.Next() {
			allVals := make([]interface{}, 0)
			step := j.Value.(*Step)

			if (*step).GetPrev == true {
				logger.WithFields(logrus.Fields{
					"rule": (*r.Destination.Plugin).Name(),
					"func": (*step.Plugin).Name(),
				}).Debug("Former func result")
				if j.Prev() == nil {
					logger.WithFields(logrus.Fields{
						"rule": (*r.Destination.Plugin).Name(),
						"err":  "FRONTS PREVIOUS IMPOSSIBLE",
					}).Error("WE CANT GET A FORMER FUNCTION IF WE ARE THE FRONT")

					return false, nil, errors.New("FRONTS PREVIOUS IMPOSSIBLE")
				}

				valTmp := j.Prev().Value.(*Step).Result.(string)
				(*step).Value = &valTmp
				logger.WithFields(logrus.Fields{
					"rule":  (*r.Destination.Plugin).Name(),
					"value": step.Value,
				}).Debug("Previous step value")

				allVals = append(allVals, *step.Value)
			}

			if (*step).MultiArgs != nil && step.MultiArgs.Len() != 0 {
				for k := step.MultiArgs.Front(); k != nil; k = k.Next() {
					for _, vals := range allVals {
						logger.WithFields(logrus.Fields{
							"rule":  (*r.Destination.Plugin).Name(),
							"value": vals,
						}).Debug("Extra values")
					}

					stepValue := k.Value.(*Step).Value
					if *stepValue != "OPERATOR" {
						eres, eerr := (*k.Value.(*Step).Plugin).Runnable(*stepValue, *record)
						if eerr != nil {
							logger.WithFields(logrus.Fields{
								"rule": (*r.Destination.Plugin).Name(),
								"err":  eerr.Error(),
							}).Error("multi param err")
							return false, nil, errors.New("multi param err")
						}

						logger.WithFields(logrus.Fields{
							"rule":  (*r.Destination.Plugin).Name(),
							"step":  stepValue,
							"value": eres,
						}).Debug("Step value and tmp value")

						allVals = append(allVals, eres)
					} else {
						stepRes := k.Value.(*Step).Result.(string)
						opsres, err := (*k.Value.(*Step).Plugin).Runnable(*step.Value, stepRes)

						logger.WithFields(logrus.Fields{
							"rule":   (*r.Destination.Plugin).Name(),
							"result": opsres,
							"err":    err,
						}).Debug("OPS Reults")
						if opsres.(bool) == false {
							logger.WithFields(logrus.Fields{
								"rule": (*r.Destination.Plugin).Name(),
							}).Debug("Condition is FALSE breaking the loop")
							return false, nil, nil
						} else {
							break
						}
					}
				}
			} else {
				if ruleProperty != "CACHE" {
					allVals = append(allVals, *step.Value)
				} else {
					logger.WithFields(logrus.Fields{
						"rule": (*r.Destination.Plugin).Name(),
					}).Debug("No all vals...")
				}
			}

			if ruleProperty != "CACHE" {
				allVals = append(allVals, *record)
			} else {
				allVals = append(allVals, *step.Value, *record)
			}

			//for _, vals := range allVals {
			//	logger.WithFields(logrus.Fields{
			//		"rule": (*r.Destination.Plugin).Name(),
			//		"value": vals,
			//	}).Debug("All the values")
			//}

			logger.WithFields(logrus.Fields{
				"rule":         (*r.Destination.Plugin).Name(),
				"stepId":       step.ID,
				"stepIdPlugin": step,
				"stepValue":    step.Value,
			}).Debug("Step debug info")

			var res interface{}

			if ruleProperty != "CACHE" && *step.Value == "OPERATOR" {
				if j.Prev() != nil {
					logger.WithFields(logrus.Fields{
						"rule":     (*r.Destination.Plugin).Name(),
						"previous": j.Prev().Value.(*Step),
					}).Debug("Predicate Previous")

					var prevRes interface{}
					var stepRes interface{}
					prevRes = j.Prev().Value.(*Step).Result
					stepRes = step.Result
					opsres, _ := (*step.Plugin).Runnable(prevRes, stepRes)
					logger.WithFields(logrus.Fields{
						"rule":   (*r.Destination.Plugin).Name(),
						"result": opsres,
					}).Debug("OPS Reults")
					if opsres.(bool) == false {
						logger.WithFields(logrus.Fields{
							"rule": (*r.Destination.Plugin).Name(),
						}).Debug("Condition is FALSE breaking the loop")
						return false, nil, nil
					} else {
						continue
					}

				} else {
					logger.WithFields(logrus.Fields{
						"rule": (*r.Destination.Plugin).Name(),
					}).Debug("BAD FORM!!!")
				}
			} else {
				if (*step).Plugin != nil {
					res, err = (*step.Plugin).Runnable(allVals...)
				}
			}

			if ruleProperty == "CACHE" && step != i.Value.(Base).Line.Front().Value.(*Step) {
				if (*i.Value.(Base).Line.Front().Value.(*Step)).Plugin != nil {
					var tmp string
					if reflect.TypeOf(res).Kind() != reflect.String {
						tmp = fmt.Sprintf("%v", res)
					} else {
						tmp = res.(string)
					}
					(*i.Value.(Base).Line.Front().Value.(*Step).Plugin).Runnable(*step.ID, tmp)
				}
			}

			if err != nil {
				logger.WithFields(logrus.Fields{
					"rule": (*r.Destination.Plugin).Name(),
					"err":  err.Error(),
				}).Error("Something bad happened in projection STEP")
				return false, nil, errors.New("something bad happened")
			}
			newStep := &Step{
				Plugin: step.Plugin,
				Value:  step.Value,
				Result: res,
				ID:     step.ID,
			}
			j.Value = newStep

			if ruleProperty == "PROJECTION" {
				if step.Plugin != nil && (*step.Plugin).Name() == "SENDALL" {
					sendAllFlag = true
					break
				}
				downstream[*step.ID] = res
			}
		}
	}

	if ruleProperty == "PROJECTION" {

		if sendAllFlag {
			logger.WithFields(logrus.Fields{
				"rule": (*r.Destination.Plugin).Name(),
			}).Debug("Send all record")
			return true, []byte((*record)), nil

		} else {
			obound, err := json.Marshal(downstream)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"rule":     (*r.Destination.Plugin).Name(),
					"property": ruleProperty,
					"err":      err.Error(),
				}).Error("Error marshalling")
				return false, nil, err
			}

			logger.WithFields(logrus.Fields{
				"rule": (*r.Destination.Plugin).Name(),
			}).Debug("Projection successfully run")

			return true, obound, nil
		}
	}

	logger.WithFields(logrus.Fields{
		"rule":     (*r.Destination.Plugin).Name(),
		"property": ruleProperty,
	}).Debug("RunProjection successfully run")

	return true, nil, nil
}
