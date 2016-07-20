package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	etcd "github.com/coreos/etcd/client"
	"github.com/gogo/protobuf/jsonpb"
	"github.com/gogo/protobuf/proto"
	"github.com/opsee/basic/clients/hugs"
	"github.com/opsee/basic/schema"
	opsee "github.com/opsee/basic/service"
	log "github.com/opsee/logrus"
	opsee_types "github.com/opsee/protobuf/opseeproto/types"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type checkCompostResponse struct {
	response interface{}
}

const (
	CheckResultTableName           = "check_results"
	CheckResultCheckIdIndexName    = "check_id-index"
	CheckResultCustomerIdIndexName = "customer_id-index"
	CheckResponseTableName         = "check_responses"

	RoutePath           = "/opsee.co/routes"
	MagicExecutionGroup = "127a7354-290e-11e6-b178-2bc1f6aefc14"
)

// ListChecks fetches Checks from Bartnet and CheckResults from Beavis
// concurrently, then zips them together. If the request to Beavis fails,
// then checks are returned without results.
func (c *Client) ListChecks(ctx context.Context, user *schema.User, checkId string) ([]*schema.Check, error) {
	var (
		responseChan = make(chan *checkCompostResponse, 2)
		// checkMap     = make(map[string][]*schema.CheckResult)
		notifMap = make(map[string][]*schema.Notification)
		wg       sync.WaitGroup
	)

	// wg.Add(1)
	// go func() {
	// 	var (
	// 		results []*schema.CheckResult
	// 		err     error
	// 	)
	//
	// 	if checkId != "" {
	// 		results, err = c.CheckResults(ctx, user, checkId)
	// 	} else {
	// 		results, err = c.AllCheckResults(ctx, user)
	// 	}
	//
	// 	if err != nil {
	// 		responseChan <- &checkCompostResponse{err}
	// 	} else {
	// 		responseChan <- &checkCompostResponse{results}
	// 	}
	//
	// 	wg.Done()
	// }()

	wg.Add(1)
	go func() {
		var (
			notifs []*hugs.Notification
			err    error
		)

		if checkId != "" {
			notifs, err = c.Hugs.ListNotificationsCheck(user, checkId)
		} else {
			notifs, err = c.Hugs.ListNotifications(user)
		}

		if err != nil {
			responseChan <- &checkCompostResponse{err}
		} else {
			responseChan <- &checkCompostResponse{notifs}
		}

		wg.Done()
	}()

	var (
		checks []*schema.Check
		err    error
	)

	if checkId != "" {
		check, err := c.Bartnet.GetCheck(user, checkId)
		if err != nil {
			log.WithError(err).Error("couldn't list checks from bartnet")
			return nil, err
		}

		checks = append(checks, check)
	} else {
		checks, err = c.Bartnet.ListChecks(user)
		if err != nil {
			log.WithError(err).Error("couldn't list checks from bartnet")
			return nil, err
		}
	}

	wg.Wait()
	close(responseChan)

	for resp := range responseChan {
		switch t := resp.response.(type) {
		// case []*schema.CheckResult:
		// 	for _, result := range t {
		// 		for _, res := range result.Responses {
		// 			if res.Reply == nil {
		// 				if res.Response == nil {
		// 					continue
		// 				}
		//
		// 				any, err := opsee_types.UnmarshalAny(res.Response)
		// 				if err != nil {
		// 					log.WithError(err).Error("couldn't list results from beavis")
		// 					return nil, err
		// 				}
		//
		// 				switch reply := any.(type) {
		// 				case *schema.HttpResponse:
		// 					res.Reply = &schema.CheckResponse_HttpResponse{reply}
		// 				case *schema.CloudWatchResponse:
		// 					res.Reply = &schema.CheckResponse_CloudwatchResponse{reply}
		// 				}
		// 			}
		// 		}
		//
		// 		if _, ok := checkMap[result.CheckId]; !ok {
		// 			checkMap[result.CheckId] = []*schema.CheckResult{result}
		// 		} else {
		// 			checkMap[result.CheckId] = append(checkMap[result.CheckId], result)
		// 		}
		// 	}
		//
		case []*hugs.Notification:
			for _, notif := range t {
				notifMap[notif.CheckId] = append(notifMap[notif.CheckId], &schema.Notification{Type: notif.Type, Value: notif.Value})
			}

		case error:
			log.WithError(t).Error("error composting checks")
		}
	}

	for _, check := range checks {
		results, err := c.CheckResults(ctx, user, check.Id)
		if err != nil {
			return nil, err
		}

		check.Results = results
		check.Notifications = notifMap[check.Id]

		if check.Spec == nil {
			if check.CheckSpec == nil {
				continue
			}

			any, err := opsee_types.UnmarshalAny(check.CheckSpec)
			if err != nil {
				log.WithError(err).Error("couldn't list checks from bartnet")
				return nil, err
			}

			switch spec := any.(type) {
			case *schema.HttpCheck:
				check.Spec = &schema.Check_HttpCheck{spec}
			case *schema.CloudWatchCheck:
				check.Spec = &schema.Check_CloudwatchCheck{spec}
			}
		}
	}

	return checks, nil
}

func (c *Client) UpsertChecks(ctx context.Context, user *schema.User, checksInput []interface{}) ([]*schema.Check, error) {
	notifs := make([]*hugs.NotificationRequest, 0, len(checksInput))
	checksResponse := make([]*schema.Check, len(checksInput))

	for i, checkInput := range checksInput {
		check, ok := checkInput.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("error decoding check input")
		}

		notifList, _ := check["notifications"].([]interface{})
		delete(check, "notifications")

		checkJson, err := json.Marshal(check)
		if err != nil {
			return nil, err
		}

		checkProto := &schema.Check{}
		err = jsonpb.Unmarshal(bytes.NewBuffer(checkJson), checkProto)
		if err != nil {
			return nil, err
		}

		var checkResponse *schema.Check

		if checkProto.Id == "" {
			checkResponse, err = c.Bartnet.CreateCheck(user, checkProto)
			if err != nil {
				return nil, err
			}
		} else {
			checkResponse, err = c.Bartnet.UpdateCheck(user, checkProto)
			if err != nil {
				return nil, err
			}
		}

		if notifList != nil {
			notif := &hugs.NotificationRequest{
				CheckId: checkResponse.Id,
			}

			for _, n := range notifList {
				nl, _ := n.(map[string]interface{})
				t, _ := nl["type"].(string)
				v, _ := nl["value"].(string)

				if t != "" && v != "" {
					// due to our crappy backend, we send a bulk request to hugs of all notifications...
					notif.Notifications = append(notif.Notifications, &hugs.Notification{
						Type:  t,
						Value: v,
					})

					// ... then we add each notification to the check object
					checkResponse.Notifications = append(checkResponse.Notifications, &schema.Notification{
						Type:  t,
						Value: v,
					})
				}
			}

			notifs = append(notifs, notif)
		}

		err = c.Hugs.CreateNotificationsMulti(user, notifs)
		if err != nil {
			return nil, err
		}

		checksResponse[i] = checkResponse
	}

	return checksResponse, nil
}

func (c *Client) DeleteChecks(ctx context.Context, user *schema.User, checksInput []interface{}) ([]string, error) {
	deleted := make([]string, 0, len(checksInput))
	for _, ci := range checksInput {
		id, ok := ci.(string)
		if !ok {
			return nil, fmt.Errorf("unable to decode check id")
		}

		err := c.Bartnet.DeleteCheck(user, id)
		if err != nil {
			continue
		}

		deleted = append(deleted, id)
	}

	return deleted, nil
}

func (c *Client) TestCheck(ctx context.Context, user *schema.User, checkInput map[string]interface{}) (*opsee.TestCheckResponse, error) {
	var (
		responses []*schema.CheckResponse
		exgroupId = user.CustomerId
	)

	checkJson, err := json.Marshal(checkInput)
	if err != nil {
		return nil, err
	}

	checkProto := &schema.Check{}
	err = jsonpb.Unmarshal(bytes.NewBuffer(checkJson), checkProto)
	if err != nil {
		return nil, err
	}

	checkProto.Interval = int32(30)

	// backwards compat with old bastion proto: TODO(mark) remove
	switch t := checkProto.Spec.(type) {
	case *schema.Check_HttpCheck:
		checkProto.CheckSpec, err = opsee_types.MarshalAny(t.HttpCheck)
	case *schema.Check_CloudwatchCheck:
		checkProto.CheckSpec, err = opsee_types.MarshalAny(t.CloudwatchCheck)
	}

	if err != nil {
		return nil, err
	}

	if checkProto.Target == nil {
		return nil, fmt.Errorf("test check is missing target")
	}

	if checkProto.Target.Type == "external_host" {
		exgroupId = MagicExecutionGroup
	}

	// use customer id or execution group id ok!!
	response, err := c.EtcdKeys.Get(ctx, path.Join(RoutePath, exgroupId), &etcd.GetOptions{
		Recursive: true,
		Quorum:    true,
	})

	if len(response.Node.Nodes) == 0 {
		return nil, fmt.Errorf("no bastions found")
	}

	// the deadline for the TestCheckRequest, this gets folded into the bastion check runner's
	// context, but i'm not sure why it's different than our grpc request context
	deadline := &opsee_types.Timestamp{}
	deadline.Scan(time.Now().Add(time.Minute))

	node := response.Node.Nodes[0]
	responseChan := make(chan *opsee.TestCheckResponse)
	errChan := make(chan error)

	// going to set a timeout for our grpc context that's a bit bigger than the
	// TestCheckRequest deadline
	ctx, _ = context.WithTimeout(ctx, time.Minute)

	go func(node *etcd.Node) {
		services := make(map[string]interface{})

		err = json.Unmarshal([]byte(node.Value), &services)
		if err != nil {
			log.WithError(err).Errorf("error unmarshaling portmapper: %#v", node.Value)
			errChan <- err
			return
		}

		if checker, ok := services["checker"].(map[string]interface{}); ok {
			checkerHost, _ := checker["hostname"].(string)
			checkerPort, _ := checker["port"].(float64)
			addr := fmt.Sprintf("%s:%d", checkerHost, int(checkerPort))

			conn, err := grpc.Dial(
				addr,
				grpc.WithInsecure(),
				grpc.WithBlock(),
				grpc.WithTimeout(3*time.Second),
			)
			if err != nil {
				log.WithError(err).Errorf("coudln't contact bastion at: %s ... ignoring", addr)
				errChan <- err
				return
			}
			log.Info("established grpc connection to bastion at: %s", addr)
			defer conn.Close()

			resp, err := opsee.NewCheckerClient(conn).TestCheck(ctx, &opsee.TestCheckRequest{Deadline: deadline, Check: checkProto})
			if err != nil {
				log.WithError(err).Errorf("got error from bastion at: %s ... ignoring", addr)
				errChan <- err
				return
			}

			responseChan <- resp
		}
	}(node)

	select {
	case resp := <-responseChan:
		responses = append(responses, resp.Responses...)
	case <-errChan:
		// idk what to do with errors here
	case <-ctx.Done():
		// idk what to do with errors here
	}

	return &opsee.TestCheckResponse{Responses: responses}, nil
}

func (c *Client) AllCheckResults(ctx context.Context, user *schema.User) ([]*schema.CheckResult, error) {
	logger := log.WithFields(log.Fields{
		"fn":          "AllCheckResults",
		"customer_id": user.CustomerId,
	})

	customerIdAv, err := dynamodbattribute.Marshal(user.CustomerId)
	if err != nil {
		logger.WithError(err).Error("Error marshalling customerID.")
		return nil, err
	}

	resultIdsResponse, err := c.Dynamo.Query(&dynamodb.QueryInput{
		TableName:              aws.String(CheckResultTableName),
		IndexName:              aws.String(CheckResultCustomerIdIndexName),
		KeyConditionExpression: aws.String("customer_id = :customer_id"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":customer_id": customerIdAv,
		},
	})
	if err != nil {
		logger.WithError(err).Error("Error querying dynamodb for customer results.")
		return nil, err
	}

	results := make([]*schema.CheckResult, len(resultIdsResponse.Items))
	for i, resultItem := range resultIdsResponse.Items {
		logger := logger.WithFields(log.Fields{
			"fn":          "AllCheckResults",
			"result_id":   aws.StringValue(resultItem["result_id"].S),
			"customer_id": user.CustomerId})
		// This is almost entirely copypasta from below, but without the
		// check id index query.
		resultGetItemResponse, err := c.Dynamo.GetItem(&dynamodb.GetItemInput{
			TableName: aws.String(CheckResultTableName),
			Key: map[string]*dynamodb.AttributeValue{
				"result_id": resultItem["result_id"],
			},
		})
		if err != nil {
			logger.WithError(err).Error("Error getting result item.")
			return nil, err
		}

		dynamoCheckResult := resultGetItemResponse.Item
		result := &schema.CheckResult{}
		if err := dynamodbattribute.UnmarshalMap(dynamoCheckResult, result); err != nil {
			logger.WithError(err).Error("Error unmarshaling check result from dynamodb")
			return nil, err
		}

		responseIds := []string{}
		err = dynamodbattribute.Unmarshal(dynamoCheckResult["responses"], &responseIds)
		if err != nil {
			logger.WithError(err).Error("Error unmarshaling response list from dynamodb result.")
			return nil, err
		}

		checkResponses := make([]*schema.CheckResponse, len(responseIds))
		for j, responseId := range responseIds {
			logger := logger.WithFields(log.Fields{
				"fn":          "AllCheckResults",
				"result_id":   aws.StringValue(resultItem["result_id"].S),
				"customer_id": user.CustomerId,
				"response_id": responseId,
			})
			responseIdAv, err := dynamodbattribute.Marshal(responseId)

			responseGetItemResponse, err := c.Dynamo.GetItem(&dynamodb.GetItemInput{
				TableName: aws.String(CheckResponseTableName),
				Key: map[string]*dynamodb.AttributeValue{
					"response_id": responseIdAv,
				},
			})
			if err != nil {
				logger.WithError(err).Error("Error getting response from dynamodb.")
				return nil, err
			}

			checkResponse := &schema.CheckResponse{}
			responseProtoAv, ok := responseGetItemResponse.Item["response_protobuf"]
			if !ok {
				err := fmt.Errorf("Response in dynamodb had no response object.")
				logger.WithError(err).Error("No protobuf for response object in dyanmodb")
				return nil, err
			}
			responseProto := []byte{}
			if err := dynamodbattribute.Unmarshal(responseProtoAv, &responseProto); err != nil {
				logger.WithError(err).Error("Error unmarshalling response protobuf from dynamodb")
				return nil, err
			}
			if err := proto.Unmarshal(responseProto, checkResponse); err != nil {
				logger.WithError(err).Error("Error unmarshaling protobuf.")
				return nil, err
			}
			checkResponses[j] = checkResponse
		}

		result.Responses = checkResponses
		results[i] = result
	}

	return results, nil
}

func (c *Client) CheckResults(ctx context.Context, user *schema.User, checkId string) ([]*schema.CheckResult, error) {
	logger := log.WithFields(log.Fields{
		"fn":          "CheckResults",
		"check_id":    checkId,
		"customer_id": user.CustomerId,
	})
	checkIdAv, err := dynamodbattribute.Marshal(checkId)
	if err != nil {
		return nil, err
	}

	// First we must query check_results-index for the result_ids for that check.
	params := &dynamodb.QueryInput{
		TableName:              aws.String(CheckResultTableName),
		IndexName:              aws.String(CheckResultCheckIdIndexName),
		KeyConditionExpression: aws.String("check_id = :check_id"),
		ExpressionAttributeValues: map[string]*dynamodb.AttributeValue{
			":check_id": checkIdAv,
		},
	}

	checkIndexResponse, err := c.Dynamo.Query(params)
	if err != nil {
		logger.WithError(err).Error("Error querying dynamodb check index.")
		return nil, err
	}

	results := make([]*schema.CheckResult, len(checkIndexResponse.Items))
	for i, resultAvMap := range checkIndexResponse.Items {
		logger := log.WithFields(log.Fields{
			"fn":          "CheckResults",
			"check_id":    checkId,
			"customer_id": user.CustomerId,
			"result_id":   aws.StringValue(resultAvMap["result_id"].S),
		})
		// Now we must call GetItem for that result_id
		resultGetItemResponse, err := c.Dynamo.GetItem(&dynamodb.GetItemInput{
			TableName: aws.String(CheckResultTableName),
			Key: map[string]*dynamodb.AttributeValue{
				"result_id": resultAvMap["result_id"],
			},
		})
		if err != nil {
			logger.WithError(err).Error("Error getting result item from dyanmodb")
			return nil, err
		}

		dynamoCheckResult := resultGetItemResponse.Item
		result := &schema.CheckResult{}
		if err := dynamodbattribute.UnmarshalMap(dynamoCheckResult, result); err != nil {
			logger.WithError(err).Error("Error unmarshalling check result from dynamodb")
			return nil, err
		}

		responseIds := []string{}
		err = dynamodbattribute.Unmarshal(dynamoCheckResult["responses"], &responseIds)
		if err != nil {
			logger.WithError(err).Error("Error unmarshalling response list from dynamodb")
			return nil, err
		}

		checkResponses := make([]*schema.CheckResponse, len(responseIds))
		for j, responseId := range responseIds {
			logger := log.WithFields(log.Fields{
				"fn":          "CheckResults",
				"check_id":    checkId,
				"customer_id": user.CustomerId,
				"result_id":   aws.StringValue(resultAvMap["result_id"].S),
				"response_id": responseId,
			})
			responseIdAv, err := dynamodbattribute.Marshal(responseId)

			responseGetItemResponse, err := c.Dynamo.GetItem(&dynamodb.GetItemInput{
				TableName: aws.String(CheckResponseTableName),
				Key: map[string]*dynamodb.AttributeValue{
					"response_id": responseIdAv,
				},
			})
			if err != nil {
				logger.WithError(err).Error("Error getting response item from dynamodb.")
				return nil, err
			}

			checkResponse := &schema.CheckResponse{}
			responseProtoAv, ok := responseGetItemResponse.Item["response_protobuf"]
			if !ok {
				err := fmt.Errorf("Response in dynamodb had no response object.")
				logger.WithError(err).Error("Empty response protobuf in dynamodb.")
				return nil, err
			}
			responseProto := []byte{}
			if err := dynamodbattribute.Unmarshal(responseProtoAv, &responseProto); err != nil {
				logger.WithError(err).Error("Error unmarshalling response protobuf from dynamodb")
				return nil, err
			}
			if err := proto.Unmarshal(responseProto, checkResponse); err != nil {
				logger.WithError(err).Error("Error unmarshalling response protobuf")
				return nil, err
			}
			checkResponses[j] = checkResponse
		}

		result.Responses = checkResponses
		results[i] = result
	}

	return results, nil
}

// Get check state transitions from cats
func (c *Client) GetCheckStateTransitions(ctx context.Context, user *schema.User, checkId string, startTime, endTime *opsee_types.Timestamp) ([]*schema.CheckStateTransition, error) {
	req := &opsee.GetCheckStateTransitionsRequest{
		CheckId:           checkId,
		CustomerId:        user.CustomerId,
		AbsoluteStartTime: startTime,
		AbsoluteEndTime:   endTime,
	}

	resp, err := c.Cats.GetCheckStateTransitions(ctx, req)
	if err != nil {
		return nil, err
	}

	return resp.Transitions, nil
}
