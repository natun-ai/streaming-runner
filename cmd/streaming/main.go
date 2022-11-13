/*
Copyright (c) 2022 RaptorML authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	grpcMiddleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcZap "github.com/grpc-ecosystem/go-grpc-middleware/logging/zap"
	grpcRetry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	raptorApi "github.com/raptor-ml/raptor/api/v1alpha1"
	_ "github.com/raptor-ml/streaming-runner/internal/brokers"
	"github.com/raptor-ml/streaming-runner/internal/manager"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	pbRuntime "go.buf.build/raptor/api-go/raptor/core/raptor/runtime/v1alpha1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/local"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"os"
	"os/signal"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"syscall"
)

// version is being overridden in build time
var version = "master"

var setupLog logr.Logger

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(clientgoscheme.Scheme))

	utilruntime.Must(raptorApi.AddToScheme(clientgoscheme.Scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	pflag.Bool("production", true, "Set as production")
	pflag.String("data-source-resource", "", "The resource name of the DataSource")
	pflag.String("data-source-namespace", "", "The namespace name of the DataSource")
	pflag.String("runtime-grpc-addr", ":60005", "The gRPC Address of the Raptor Runtime")
	pflag.Parse()
	must(viper.BindPFlags(pflag.CommandLine))

	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	zl := logger()
	logger := zapr.NewLogger(zl)
	setupLog = logger.WithName("setup")

	if viper.GetString("data-source-resource") == "" || viper.GetString("data-source-namespace") == "" {
		must(fmt.Errorf("`data-source-resource` and `data-source-namespace` are required"))
	}

	cc, err := grpc.Dial(
		viper.GetString("runtime-grpc-addr"),
		grpc.WithStreamInterceptor(grpcMiddleware.ChainStreamClient(
			grpcZap.StreamClientInterceptor(zl.Named("runtime")),
			grpcRetry.StreamClientInterceptor(),
		)),
		grpc.WithUnaryInterceptor(grpcMiddleware.ChainUnaryClient(
			grpcZap.UnaryClientInterceptor(zl.Named("runtime")),
			grpcRetry.UnaryClientInterceptor(),
		)),
		grpc.WithTransportCredentials(local.NewCredentials()),
	)
	must(err)
	runtime := pbRuntime.NewRuntimeServiceClient(cc)

	src := client.ObjectKey{
		Name:      viper.GetString("data-source-resource"),
		Namespace: viper.GetString("data-source-namespace"),
	}
	mgr, err := manager.New(src, runtime, ctrl.GetConfigOrDie(), logger.WithName("manager"))
	must(err)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	setupLog.Info("Starting streaming-runner", "version", version)
	err = mgr.Start(ctx)
	must(err)
	defer cancel()

}
func logger() *zap.Logger {
	var l *zap.Logger
	var err error
	if viper.GetBool("production") {
		l, err = zap.NewProduction()
	} else {
		l, err = zap.NewDevelopment()
	}
	must(err)

	return l
}

func must(err error) {
	if err != nil {
		if setupLog.GetSink() != nil {
			setupLog.Error(err, "failed to start")
		} else {
			fmt.Println(err)
		}
		os.Exit(1)
	}
}
