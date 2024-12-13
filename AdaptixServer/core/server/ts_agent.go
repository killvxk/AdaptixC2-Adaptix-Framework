package server

import (
	"AdaptixServer/core/adaptix"
	"AdaptixServer/core/utils/krypt"
	"AdaptixServer/core/utils/logs"
	"AdaptixServer/core/utils/safe"
	"AdaptixServer/core/utils/tformat"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (ts *Teamserver) TsAgentUpdateData(newAgentObject []byte) error {
	var (
		agent        *Agent
		err          error
		newAgentData adaptix.AgentData
	)

	err = json.Unmarshal(newAgentObject, &newAgentData)
	if err != nil {
		return err
	}

	value, ok := ts.agents.Get(newAgentData.Id)
	if !ok {
		return errors.New("Agent does not exist")
	}

	agent, _ = value.(*Agent)
	agent.Data.Sleep = newAgentData.Sleep
	agent.Data.Jitter = newAgentData.Jitter
	agent.Data.Elevated = newAgentData.Elevated
	agent.Data.Username = newAgentData.Username

	err = ts.DBMS.DbAgentUpdate(agent.Data)
	if err != nil {
		logs.Error(err.Error())
	}

	packetNew := CreateSpAgentUpdate(agent.Data)
	ts.TsSyncAllClients(packetNew)

	return nil
}

func (ts *Teamserver) TsAgentRequest(agentCrc string, agentId string, beat []byte, bodyData []byte, listenerName string, ExternalIP string) ([]byte, error) {
	var (
		agentName      string
		data           []byte
		respData       []byte
		err            error
		agent          *Agent
		agentData      adaptix.AgentData
		agentTasksData [][]byte
		agentBuffer    bytes.Buffer
	)

	value, ok := ts.agent_types.Get(agentCrc)
	if !ok {
		return nil, fmt.Errorf("agent type %v does not exists", agentCrc)
	}
	agentName = value.(string)

	/// CREATE OR GET AGENT

	value, ok = ts.agents.Get(agentId)
	if !ok {
		data, err = ts.Extender.ExAgentCreate(agentName, beat)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(data, &agentData)
		if err != nil {
			return nil, err
		}

		agentData.Crc = agentCrc
		agentData.Name = agentName
		agentData.Id = agentId
		agentData.Listener = listenerName
		agentData.ExternalIP = ExternalIP
		agentData.CreateTime = time.Now().Unix()
		agentData.LastTick = int(time.Now().Unix())
		agentData.Tags = ""

		agent = &Agent{
			Data:        agentData,
			TasksQueue:  safe.NewSlice(),
			Tasks:       safe.NewMap(),
			ClosedTasks: safe.NewMap(),
		}

		ts.agents.Put(agentData.Id, agent)

		err = ts.DBMS.DbAgentInsert(agentData)
		if err != nil {
			logs.Error(err.Error())
		}

		packetNew := CreateSpAgentNew(agentData)
		ts.TsSyncAllClients(packetNew)

		message := fmt.Sprintf("New '%v' (%v) executed on '%v @ %v.%v' (%v)", agentData.Name, agentData.Id, agentData.Username, agentData.Computer, agentData.Domain, agentData.InternalIP)
		packet2 := CreateSpEvent(EVENT_AGENT_NEW, message)
		ts.TsSyncAllClients(packet2)
		ts.events.Put(packet2)

	} else {
		agent, _ = value.(*Agent)
	}

	packetTick := CreateSpAgentTick(agent.Data.Id)
	ts.TsSyncAllClients(packetTick)

	/// PROCESS RECEIVED DATA FROM AGENT

	_ = json.NewEncoder(&agentBuffer).Encode(agent.Data)
	if len(bodyData) > 4 {
		_, _ = ts.Extender.ExAgentProcessData(agentName, agentBuffer.Bytes(), bodyData)
	}

	/// SEND NEW DATA TO AGENT

	if agent.TasksQueue.Len() > 0 {
		respData, err = ts.Extender.ExAgentPackData(agentName, agentBuffer.Bytes(), agentTasksData)
		if err != nil {
			return nil, err
		}

		message := fmt.Sprintf("Agent called server, sent [%v]", tformat.SizeBytesToFormat(uint64(len(respData))))
		ts.TsAgentConsoleOutput(agentId, CONSOLE_OUT_INFO, message, "")
	}

	return respData, nil
}

func (ts *Teamserver) TsAgentCommand(agentName string, agentId string, username string, cmdline string, args map[string]any) error {
	var (
		err         error
		agentObject bytes.Buffer
		messageInfo string
		agent       *Agent
		taskData    adaptix.TaskData
		data        []byte
	)

	if ts.agent_configs.Contains(agentName) {

		value, ok := ts.agents.Get(agentId)
		if ok {

			agent, _ = value.(*Agent)
			_ = json.NewEncoder(&agentObject).Encode(agent.Data)

			data, messageInfo, err = ts.Extender.ExAgentCommand(agentName, agentObject.Bytes(), args)
			if err != nil {
				return err
			}

			err = json.Unmarshal(data, &taskData)
			if err != nil {
				return err
			}

			if taskData.TaskId == "" {
				taskData.TaskId, _ = krypt.GenerateUID(8)
			}
			taskData.CommandLine = cmdline
			taskData.AgentId = agentId
			taskData.User = username
			taskData.StartDate = time.Now().Unix()

			agent.TasksQueue.Put(taskData)

			packet := CreateSpAgentTaskCreate(taskData)
			ts.TsSyncAllClients(packet)

			if len(messageInfo) > 0 {
				ts.TsAgentConsoleOutput(agentId, CONSOLE_OUT_INFO, messageInfo, "")
			}

		} else {
			return fmt.Errorf("agent '%v' does not exist", agentId)
		}
	} else {
		return fmt.Errorf("agent %v not registered", agentName)
	}

	return nil
}

func (ts *Teamserver) TsAgentConsoleOutput(agentId string, messageType int, message string, clearText string) {
	packet := CreateSpAgentConsoleOutput(agentId, messageType, message, clearText)
	ts.TsSyncAllClients(packet)
}

func (ts *Teamserver) TsAgentRemove(agentId string) error {
	_, ok := ts.agents.GetDelete(agentId)
	if !ok {
		return fmt.Errorf("agent '%v' does not exist", agentId)
	}

	err := ts.DBMS.DbAgentDelete(agentId)
	if err != nil {
		logs.Error(err.Error())
	} else {
		ts.DBMS.DbTaskDelete("", agentId)
	}

	packet := CreateSpAgentRemove(agentId)
	ts.TsSyncAllClients(packet)

	return nil
}

func (ts *Teamserver) TsAgentSetTag(agentId string, tag string) error {
	value, ok := ts.agents.Get(agentId)
	if !ok {
		return errors.New("Agent does not exist")
	}

	agent, _ := value.(*Agent)
	agent.Data.Tags = tag

	err := ts.DBMS.DbAgentUpdate(agent.Data)
	if err != nil {
		logs.Error(err.Error())
	}

	packetNew := CreateSpAgentUpdate(agent.Data)
	ts.TsSyncAllClients(packetNew)

	return nil
}
