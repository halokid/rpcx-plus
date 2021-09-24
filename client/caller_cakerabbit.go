package client
/*
implement caller for cakeRabbit service
*/

import (
  "github.com/halokid/ColorfulRabbit"
  "github.com/halokid/rpcx-plus/log"
  "github.com/msgpack-rpc/msgpack-rpc-go/rpc"
  "github.com/spf13/cast"
  logx "log"
  "net"
)

func (c *caller) invokeCake(nodeAddr string, svc string, call string, bodyTran map[string]interface{}, psKey []string) ([]byte, error) {
  conn, err := net.Dial("tcp", nodeAddr)
  defer conn.Close()
  ColorfulRabbit.CheckError(err, "invoke cakeRabbit service error")
  if err != nil {
    return []byte{}, err
  }

  client := rpc.NewSession(conn, true)
  var args []interface{}
  //paramsIndex := []string{"pageIndex", "pageSize", "keyword"}
  paramsIndex := psKey
  //for _, arg := range bodyTran {
    //args = append(args, arg)
    //args = append(args, cast.ToString(arg))
  //}

  for _, key := range paramsIndex {
    if val, ok := bodyTran[key]; ok {
      args = append(args, cast.ToString(val))
    }
  }

  logx.Printf("invokeCake args ----------- %+v", args)
  //rsp, err := client.Send("say_hello", "foo")
  //rsp, err := client.Send(call, "foo")
  //rsp, err := client.Send(call, 3)
  // todo: change type reflect in cakeRabbit, not in golang, so bodyTran all params use string type
  rsp, err := client.Send(call, args...)
  ColorfulRabbit.CheckError(err, "invoke cakeRabbit service call error")
  if err != nil {
    return []byte{}, err
  }
  log.ADebug.Print("invokeCake rsp -------", rsp)
  rspS := rsp.String()
  return []byte(rspS), nil
}



