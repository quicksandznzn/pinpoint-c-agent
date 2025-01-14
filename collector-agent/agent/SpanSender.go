package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/wrappers"
	"github.com/pinpoint-apm/pinpoint-c-agent/collector-agent/common"
	v1 "github.com/pinpoint-apm/pinpoint-c-agent/collector-agent/pinpoint-grpc-idl/proto/v1"
	"github.com/spaolacci/murmur3"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type ApiIdMap map[string]interface{}

var unique_id_count = int32(1)

type SpanSender struct {
	sequenceId    int32
	idMap         ApiIdMap
	Md            metadata.MD
	exitCh        chan bool
	spanMessageCh chan *v1.PSpanMessage
	spanRespCh    chan int32
	wg            sync.WaitGroup
}

func CreateSpanSender(base metadata.MD, exitCh chan bool) *SpanSender {
	sender := &SpanSender{
		Md: base, exitCh: exitCh,
		idMap: make(ApiIdMap),
	}
	return sender
}

func (spanSender *SpanSender) Stop() {
	log.Warn("Try to close spanSend goroutine")
	close(spanSender.spanMessageCh)
	close(spanSender.spanRespCh)
	spanSender.wg.Wait()
	log.Warn("Span sender goroutine exit")
}

func (spanSender *SpanSender) senderMain() {
	config := common.GetConfig()
	conn, err := common.CreateGrpcConnection(config.SpanAddress)
	if err != nil {
		log.Warnf("connect:%s failed. %s", config.SpanAddress, err)
		return
	}
	defer conn.Close()
	client := v1.NewSpanClient(conn)

	ctx := metadata.NewOutgoingContext(context.Background(), spanSender.Md)

	stream, err := client.SendSpan(ctx)
	if err != nil {
		log.Warnf("create stream failed. %s", err)
		return
	}
	defer stream.CloseSend()

	for span := range spanSender.spanMessageCh {
		log.Debugf("send %v", span)

		if err := stream.Send(span); err != nil {
			log.Warnf("send span failed with:%s", err)
			// response the stream is not available
			spanSender.spanRespCh <- 500
			return
		}
	}
}

func (spanSender *SpanSender) sendThread() {
	defer spanSender.wg.Done()

	spanSender.wg.Add(1)
	for {
		spanSender.senderMain()
		config := common.GetConfig()
		if common.WaitChannelEvent(spanSender.exitCh, config.SpanTimeWaitSec) == common.E_AGENT_STOPPING {
			break
		}
	}
	log.Info("sendThread exit")
}

func (spanSender *SpanSender) Init() {
	// spanSender.sqlMeta = MetaData{MetaDataType: common.META_SQL_UID, IDMap: make(PARAMS_TYPE), Sender: spanSender}
	// spanSender.apiMeta = MetaData{MetaDataType: common.META_API, IDMap: make(PARAMS_TYPE), Sender: spanSender}
	// spanSender.stringMeta = MetaData{MetaDataType: common.META_STRING, IDMap: make(PARAMS_TYPE), Sender: spanSender}

	spanSender.spanMessageCh = make(chan *v1.PSpanMessage, common.GetConfig().AgentChannelSize)
	spanSender.spanRespCh = make(chan int32, 1)
	log.Debug("SpanSender::Init span spanSender thread start")
	for i := int32(0); i < common.GetConfig().SpanStreamParallelismSize; i++ {
		go spanSender.sendThread()
	}
	log.Debug("SpanSender::Init done")
}

func (spanSender *SpanSender) cleanAllMetaData() {
	log.Info("Clean all metaData")
	spanSender.idMap = make(ApiIdMap)
}

func (spanSender *SpanSender) makePinpointSpanEv(genSpan *v1.PSpan, spanEv *TSpanEvent, depth int32) error {
	if pbSpanEv, err := spanSender.createPinpointSpanEv(spanEv); err == nil {
		pbSpanEv.Sequence = spanSender.sequenceId
		spanSender.sequenceId += 1
		pbSpanEv.Depth = depth
		genSpan.SpanEvent = append(genSpan.SpanEvent, pbSpanEv)
		for _, call := range spanEv.Calls {
			spanSender.makePinpointSpanEv(genSpan, &call, depth+1)
		}
		return nil
	} else {
		return err
	}
}

func (spanSender *SpanSender) getMetaApiId(name string, metaType int32) int32 {
	id, ok := spanSender.idMap[name]
	if ok {
		return id.(int32)
	} else {
		unique_id_count += 1
		spanSender.idMap[name] = unique_id_count
		spanSender.SenderGrpcMetaData(name, metaType)
		return unique_id_count
	}
}

func (spanSender *SpanSender) getSqlUidMetaApiId(name string) []byte {
	id, ok := spanSender.idMap[name]
	if ok {
		return id.([]byte)
	} else {
		h1, h2 := murmur3.Sum128([]byte(name))
		id := []byte(strconv.FormatUint(h1, 16) + strconv.FormatUint(h2, 16))
		spanSender.idMap[name] = id
		spanSender.SenderGrpcMetaData(name, common.META_Sql_uid_api)
		return id
	}
}

func (spanSender *SpanSender) createPinpointSpanEv(spanEv *TSpanEvent) (*v1.PSpanEvent, error) {
	pbSpanEv := &v1.PSpanEvent{}

	pbSpanEv.ApiId = spanSender.getMetaApiId(spanEv.Name, common.META_Default_api)

	if len(spanEv.ExceptionInfo) > 0 {
		id := spanSender.getMetaApiId("___EXP___", common.META_String_api)
		pbSpanEv.ExceptionInfo = &v1.PIntStringValue{}
		pbSpanEv.ExceptionInfo.IntValue = id
		stringValue := wrapperspb.StringValue{Value: spanEv.ExceptionInfo}
		pbSpanEv.ExceptionInfo.StringValue = &stringValue
	}

	nextEv := v1.PMessageEvent{
		DestinationId: spanEv.DestinationId,
		NextSpanId:    spanEv.NextSpanId,
		EndPoint:      spanEv.EndPoint,
	}

	pbSpanEv.NextEvent = &v1.PNextEvent{
		Field: &v1.PNextEvent_MessageEvent{
			MessageEvent: &nextEv},
	}

	pbSpanEv.StartElapsed = spanEv.GetStartElapsed()

	pbSpanEv.EndElapsed = spanEv.GetEndElapsed()

	pbSpanEv.ServiceType = spanEv.ServiceType
	for _, ann := range spanEv.Clues {
		iColon := strings.Index(ann, ":")
		if value, err := strconv.ParseInt(ann[0:iColon], 10, 32); err == nil {
			stringValue := v1.PAnnotationValue_StringValue{StringValue: ann[iColon+1:]}

			v := v1.PAnnotationValue{
				Field: &stringValue,
			}
			ann := v1.PAnnotation{
				Key:   int32(value),
				Value: &v,
			}
			pbSpanEv.Annotation = append(pbSpanEv.Annotation, &ann)
		}
	}

	if len(spanEv.SqlMeta) > 0 {
		id := spanSender.getSqlUidMetaApiId(spanEv.SqlMeta)
		sqlByteSv := &v1.PBytesStringStringValue{
			BytesValue: id,
			StringValue1: &wrappers.StringValue{
				Value: spanEv.SqlMeta,
			},
		}
		pbSpanEv.Annotation = append(pbSpanEv.Annotation, &v1.PAnnotation{
			Key: 25,
			Value: &v1.PAnnotationValue{
				Field: &v1.PAnnotationValue_BytesStringStringValue{
					BytesStringStringValue: sqlByteSv,
				},
			},
		})
	}

	return pbSpanEv, nil
}

func (spanSender *SpanSender) makePinpointSpan(span *TSpan) (*v1.PSpan, error) {
	spanSender.sequenceId = 0
	pbSpan := &v1.PSpan{}
	pbSpan.Version = 1
	pbSpan.ApiId = spanSender.getMetaApiId(span.GetAppid(), common.META_Web_request_api)

	pbSpan.ServiceType = span.ServerType

	pbSpan.ApplicationServiceType = span.GetAppServerType()

	pbSpan.ParentSpanId = span.ParentSpanId

	tidFormat := strings.Split(span.TransactionId, "^")
	agentId, startTime, sequenceId := tidFormat[0], tidFormat[1], tidFormat[2]
	transactionId := v1.PTransactionId{AgentId: agentId}

	if value, err := strconv.ParseInt(startTime, 10, 64); err == nil {
		transactionId.AgentStartTime = value
	}

	if value, err := strconv.ParseInt(sequenceId, 10, 64); err == nil {
		transactionId.Sequence = value
	}
	pbSpan.TransactionId = &transactionId

	pbSpan.SpanId = span.SpanId

	pbSpan.StartTime = span.GetStartTime()

	pbSpan.Elapsed = span.GetElapsedTime()

	parentInfo := v1.PParentInfo{
		ParentApplicationName: span.ParentApplicationName,
		ParentApplicationType: span.ParentAppServerType,
		AcceptorHost:          span.AcceptorHost,
	}

	acceptEv := v1.PAcceptEvent{Rpc: span.Uri, EndPoint: span.EndPoint, RemoteAddr: span.RemoteAddr, ParentInfo: &parentInfo}

	pbSpan.AcceptEvent = &acceptEv
	// changes: ERRs's priority bigger EXP, so ERR will replace EXP
	if len(span.ExceptionInfo) > 0 {
		id := spanSender.getMetaApiId("___EXP___", common.META_String_api)
		stringValue := wrapperspb.StringValue{Value: span.ExceptionInfo}
		pbSpan.ExceptionInfo = &v1.PIntStringValue{IntValue: id,
			StringValue: &stringValue}
	}

	if span.ErrorInfo != nil {
		id := spanSender.getMetaApiId("___ERR___", common.META_String_api)
		pbSpan.Err = 1 // mark as an error
		pbSpan.ExceptionInfo = &v1.PIntStringValue{
			IntValue: id,
			StringValue: &wrapperspb.StringValue{
				Value: span.ErrorInfo.Msg}}

	}

	for _, annotation := range span.Clues {
		iColon := strings.Index(annotation, ":")
		if iColon > 0 {
			if value, err := strconv.ParseInt(annotation[0:iColon], 10, 32); err == nil {
				stringValue := v1.PAnnotationValue_StringValue{StringValue: annotation[iColon+1:]}
				pAnn := v1.PAnnotationValue{
					Field: &stringValue,
				}
				ann := v1.PAnnotation{
					Key:   int32(value),
					Value: &pAnn,
				}
				pbSpan.Annotation = append(pbSpan.Annotation, &ann)
			}
		}
	}

	// collector data from nginx-header
	if len(span.NginxHeader) > 0 {
		pvalue := v1.PAnnotationValue_LongIntIntByteByteStringValue{
			LongIntIntByteByteStringValue: &v1.PLongIntIntByteByteStringValue{},
		}
		pvalue.LongIntIntByteByteStringValue.IntValue1 = 2
		ngFormat := common.ParseStringField(span.NginxHeader)
		if value, OK := ngFormat["D"]; OK {
			if value, err := common.ParseDotFormatToTime(value); err == nil {
				pvalue.LongIntIntByteByteStringValue.IntValue2 = int32(value)
			}
		}
		if value, OK := ngFormat["t"]; OK {
			if value, err := common.ParseDotFormatToTime(value); err == nil {
				pvalue.LongIntIntByteByteStringValue.LongValue = value
			}
		}

		annotation := v1.PAnnotation{
			Key: 300,
			Value: &v1.PAnnotationValue{
				Field: &pvalue,
			},
		}
		pbSpan.Annotation = append(pbSpan.Annotation, &annotation)
	}
	// collect data from apache-header
	if len(span.ApacheHeader) > 0 {
		pvalue := v1.PAnnotationValue_LongIntIntByteByteStringValue{
			LongIntIntByteByteStringValue: &v1.PLongIntIntByteByteStringValue{},
		}
		pvalue.LongIntIntByteByteStringValue.IntValue1 = 3
		npAr := common.ParseStringField(span.ApacheHeader)
		if value, OK := npAr["i"]; OK {
			if value, err := strconv.ParseInt(value, 10, 32); err == nil {
				pvalue.LongIntIntByteByteStringValue.ByteValue1 = int32(value)
			}
		}
		if value, OK := npAr["b"]; OK {
			if value, err := strconv.ParseInt(value, 10, 32); err == nil {
				pvalue.LongIntIntByteByteStringValue.ByteValue2 = int32(value)
			}
		}
		if value, OK := npAr["D"]; OK {
			if value, err := strconv.ParseInt(value, 10, 32); err == nil {
				pvalue.LongIntIntByteByteStringValue.IntValue2 = int32(value)
			}
		}
		if value, OK := npAr["t"]; OK {
			if value, err := strconv.ParseInt(value, 10, 64); err == nil {
				pvalue.LongIntIntByteByteStringValue.LongValue = value / 1000
			}
		}

		ann := v1.PAnnotation{
			Key: 300,
			Value: &v1.PAnnotationValue{
				Field: &pvalue,
			},
		}

		pbSpan.Annotation = append(pbSpan.Annotation, &ann)
	}

	return pbSpan, nil
}

func (spanSender *SpanSender) makeSpan(span *TSpan) (*v1.PSpan, error) {
	if pspan, err := spanSender.makePinpointSpan(span); err == nil {
		for _, call := range span.Calls {
			spanSender.makePinpointSpanEv(pspan, &call, 1)
		}
		return pspan, nil
	} else {
		return nil, err
	}
}

func (spanSender *SpanSender) Interceptor(span *TSpan) bool {
	log.Debug("span spanSender interceptor")
	if pbSpan, err := spanSender.makeSpan(span); err == nil {
		// send channel
		spanSender.spanMessageCh <- &v1.PSpanMessage{
			Field: &v1.PSpanMessage_Span{
				Span: pbSpan,
			},
		}
		// recv the channel status
		select {
		case statusCode := <-spanSender.spanRespCh:
			log.Warnf("span send stream is offline statusCode:%d, clear all string/sql/api meta data", statusCode)
			spanSender.cleanAllMetaData()
		case <-time.After(0 * time.Second):
			// do nothing, just go on
		}
	} else {
		log.Warnf("SpanSender::Interceptor return err:%s", err)
	}
	return true
}

func (spanSender *SpanSender) SenderGrpcMetaData(name string, metaType int32) error {
	config := common.GetConfig()
	conn, err := common.CreateGrpcConnection(config.AgentAddress)
	if err != nil {
		log.Warnf("connect:%s failed. %s", config.AgentAddress, err)
		return errors.New("SenderGrpcMetaData: connect failed")
	}

	defer conn.Close()
	client := v1.NewMetadataClient(conn)

	ctx, cancel := common.BuildPinpointCtx(config.MetaDataTimeWaitSec, spanSender.Md)
	defer cancel()

	switch metaType {
	case common.META_Default_api:
		{
			id := spanSender.idMap[name].(int32)
			apiMeta := v1.PApiMetaData{ApiId: id, ApiInfo: name, Type: common.API_DEFAULT}

			if _, err = client.RequestApiMetaData(ctx, &apiMeta); err != nil {
				log.Warnf("agentOnline api meta failed %s", err)
				return errors.New("SenderGrpcMetaData: PApiMetaData failed")
			}
		}

	case common.META_Web_request_api:
		{
			id := spanSender.idMap[name].(int32)
			apiMeta := v1.PApiMetaData{ApiId: id, ApiInfo: name, Type: common.API_WEB_REQUEST}

			if _, err = client.RequestApiMetaData(ctx, &apiMeta); err != nil {
				log.Warnf("agentOnline api meta failed %s", err)
				return errors.New("SenderGrpcMetaData: PApiMetaData failed")
			}
		}
	case common.META_String_api:
		{
			id := spanSender.idMap[name].(int32)
			metaMeta := v1.PStringMetaData{
				StringId:    id,
				StringValue: name,
			}

			if _, err = client.RequestStringMetaData(ctx, &metaMeta); err != nil {
				log.Warnf("agentOnline api meta failed %s", err)
				return errors.New("SenderGrpcMetaData: RequestStringMetaData failed")
			}
		}

	case common.META_Sql_uid_api:
		{
			id := spanSender.idMap[name].([]byte)
			sqlUidMeta := v1.PSqlUidMetaData{
				SqlUid: id,
				Sql:    name,
			}
			if _, err = client.RequestSqlUidMetaData(ctx, &sqlUidMeta); err != nil {
				log.Warnf("agentOnline api meta failed %s", err)
				return errors.New("SenderGrpcMetaData: RequestSqlUidMetaData failed")
			}
		}
	default:
		log.Warnf("SenderGrpcMetaData: No such Type:%d", metaType)
	}

	log.Debugf("send metaData %s", name)
	return nil
}
