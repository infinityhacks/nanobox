package dev

import (
  "encoding/json"
  "fmt"
  "net"
  "os"

  dockType "github.com/docker/engine-api/types"
  "github.com/nanobox-io/golang-docker-client"
  "github.com/nanobox-io/nanobox-boxfile"
  "github.com/nanobox-io/nanobox-golang-stylish"

  "github.com/nanobox-io/nanobox/models"
  "github.com/nanobox-io/nanobox/processor"
  "github.com/nanobox-io/nanobox/provider"
  "github.com/nanobox-io/nanobox/util"
  "github.com/nanobox-io/nanobox/util/config"
  "github.com/nanobox-io/nanobox/util/counter"
  "github.com/nanobox-io/nanobox/util/data"
  "github.com/nanobox-io/nanobox/util/dhcp"
  "github.com/nanobox-io/nanobox/util/locker"
  "github.com/nanobox-io/nanobox/util/print"
)

// processDevConsole ...
type processDevConsole struct {
  control   processor.ProcessControl
  app       models.App
  boxfile   models.Boxfile
  localIP   net.IP
  image     string
  container dockType.ContainerJSON
}

//
func init() {
  processor.Register("dev_console", devConsoleFn)
}

//
func devConsoleFn(control processor.ProcessControl) (processor.Processor, error) {
  // confirm the provider is an accessable one that we support.
  return &processDevConsole{control: control}, nil
}

//
func (devConsole processDevConsole) Results() processor.ProcessControl {
  return devConsole.control
}

//
func (devConsole *processDevConsole) Process() error {

  // this is bad... we should probably print a pretty message explaining that the
  // app was left in a bad state and needs to be reset
  defer func() {
    if err := devConsole.teardown(); err != nil {
      return
    }
  }()

  // run the share init which gives access to docker
  if err := processor.Run("share_init", devConsole.control); err != nil {
    return err
  }

  //
  if err := devConsole.loadApp(); err != nil {
    return err
  }

  //
  if err := devConsole.setup(); err != nil {
    // todo: how to display this?
    return err
  }

  //
  if err := devConsole.printMOTD(); err != nil {
    return err
  }

  //
  if err := devConsole.runConsole(); err != nil {
    // todo: how to display this?
    return err
  }

  return nil
}

// loadApp loads the app from the db
func (devConsole *processDevConsole) loadApp() error {

  if err := data.Get("apps", config.AppName(), &devConsole.app); err != nil {
    return err
  }

  return nil
}

// setup ...
func (devConsole *processDevConsole) setup() error {

  // let anyone else know we're using the provider
  counter.Increment(config.AppName() + "_dev")

  // establish a local lock to ensure we're the only ones bringing up the
  // dev container. Also, we need to ensure the lock is released even in we error
  locker.LocalLock()
  defer locker.LocalUnlock()

  //
  if err := devConsole.loadBoxfile(); err != nil {
    return err
  }

  //
  if !isDevRunning() {

    //
    if err := devConsole.setImage(); err != nil {
      return err
    }

    //
    if err := devConsole.downloadImage(); err != nil {
      return err
    }

    //
    if err := devConsole.reserveIP(); err != nil {
      return err
    }

    //
    if err := devConsole.launchContainer(); err != nil {
      return err
    }

    //
    if err := devConsole.attachNetwork(); err != nil {
      return err
    }

    //
    if _, err := util.Exec(devConsole.container.ID, "user", config.UserPayload(), processor.ExecWriter()); err != nil {
      return err
    }

    //
    if _, err := util.Exec(devConsole.container.ID, "dev", devConsole.devPayload(), processor.ExecWriter()); err != nil {
      return err
    }
  }

  return nil
}

// teardown ...
func (devConsole *processDevConsole) teardown() error {

  counter.Decrement(config.AppName() + "_dev")

  // establish a local app lock to ensure we're the only ones bringing
  // down the app platform. Also ensure that we release it even if we error
  locker.LocalLock()
  defer locker.LocalUnlock()

  //
  if devIsUnused() {

    //
    if err := devConsole.removeContainer(); err != nil {
      return err
    }

    //
    if err := devConsole.detachNetwork(); err != nil {
      return err
    }

    //
    if err := devConsole.releaseIP(); err != nil {
      return err
    }
  }

  return nil
}

// loadBoxfile loads the build boxfile from the database
func (devConsole *processDevConsole) loadBoxfile() error {

  if err := data.Get(config.AppName()+"_meta", "build_boxfile", &devConsole.boxfile); err != nil {
    return err
  }

  return nil
}

// setImage sets the image to use for the dev container
func (devConsole *processDevConsole) setImage() error {
  boxfile := boxfile.New(devConsole.boxfile.Data)

  devConsole.image = boxfile.Node("build").StringValue("image")

  if devConsole.image == "" {
    devConsole.image = "nanobox/dev"
  }

  return nil
}

// downloadImage downloads the dev docker image
func (devConsole *processDevConsole) downloadImage() error {
  if !docker.ImageExists(devConsole.image) {
    prefix := fmt.Sprintf("%s+ Pulling %s -", stylish.GenerateNestedPrefix(devConsole.control.DisplayLevel+1), devConsole.image)
    if _, err := docker.ImagePull(devConsole.image, &print.DockerPercentDisplay{Prefix: prefix}); err != nil {
      return err
    }
  }

  return nil
}

// reserveIP reserves a local IP for the build container
func (devConsole *processDevConsole) reserveIP() error {
  IP, err := dhcp.ReserveLocal()
  if err != nil {
    return err
  }

  devConsole.localIP = IP

  return nil
}

// releaseIP releases a local IP back into the pool
func (devConsole *processDevConsole) releaseIP() error {
  return dhcp.ReturnIP(devConsole.localIP)
}

// launchContainer starts the dev container
func (devConsole *processDevConsole) launchContainer() error {
  // parse the boxfile data
  boxfile := boxfile.New(devConsole.boxfile.Data)
  appName := config.AppName()

  config := docker.ContainerConfig{
    Name:    fmt.Sprintf("nanobox-%s-dev", appName),
    Image:   devConsole.image, // this will need to be configurable some time
    Network: "virt",
    IP:      devConsole.localIP.String(),
    Binds: []string{
      fmt.Sprintf("%s%s/code:/app", provider.HostShareDir(), appName),
      fmt.Sprintf("%s%s/build:/data", provider.HostMntDir(), appName),
      fmt.Sprintf("%s%s/cache:/mnt/cache", provider.HostMntDir(), appName),
    },
  }

  //
  for _, libDir := range boxfile.Node("code.build").StringSliceValue("lib_dirs") {
    path := fmt.Sprintf("/mnt/sda1/%s/cache/lib_dirs/%s:/app/%s", appName, libDir, libDir)
    config.Binds = append(config.Binds, path)
  }

  // start container
  container, err := docker.CreateContainer(config)

  if err != nil {
    return err
  }

  devConsole.container = container

  return nil
}

// removeContainer will lookup the dev container and remove it
func (devConsole *processDevConsole) removeContainer() error {

  name := fmt.Sprintf("nanobox-%s-dev", config.AppName())

  // grab the container info
  container, err := docker.GetContainer(name)

  if err != nil {
    return err
  }

  if err := docker.ContainerRemove(container.ID); err != nil {
    return err
  }

  return nil
}

// runUserHook runs the user hook in the dev container
func (devConsole *processDevConsole) devPayload() string {
  rtn := map[string]interface{}{}
  envVars := models.EnvVars{}
  data.Get(config.AppName()+"_meta", "env", &envVars)
  rtn["env"] = envVars
  bytes, _ := json.Marshal(rtn)
  return string(bytes)
}

// runConsole will establish a console within the dev container
func (devConsole *processDevConsole) runConsole() error {

  config := processor.ProcessControl{
    Env: devConsole.control.Env,
    Verbose: devConsole.control.Verbose,
    Meta: map[string]string{
      "container": fmt.Sprintf("nanobox-%s-dev", config.AppName()),
      "cwd":       devConsole.cwd(),
      "shell":     "zsh",
    },
  }

  if err := processor.Run("share_console", config); err != nil {
    return err
  }

  return nil
}

// cwd sets the cwd from the boxfile or provides a sensible default
func (devConsole *processDevConsole) cwd() string {
  // parse the boxfile data
  boxfile := boxfile.New(devConsole.boxfile.Data)

  if boxfile.Node("dev").StringValue("cwd") != "" {
    return boxfile.Node("dev").StringValue("cwd")
  }

  return "/app"
}

// printMOTD prints the motd with information for the user to connect
func (devConsole *processDevConsole) printMOTD() error {

  // fetch the dev ip
  devIP := devConsole.app.GlobalIPs["dev"]

  os.Stderr.WriteString(fmt.Sprintf(`
                                   **
                                ********
                             ***************
                          *********************
                            *****************
                          ::    *********    ::
                             ::    ***    ::
                           ++   :::   :::   ++
                              ++   :::   ++
                                 ++   ++
                                    +
                    _  _ ____ _  _ ____ ___  ____ _  _
                    |\ | |__| |\ | |  | |__) |  |  \/
                    | \| |  | | \| |__| |__) |__| _/\_

--------------------------------------------------------------------------------
+ You are in a virtual machine (vm)
+ Your local source code has been mounted into the vm
+ Changes to your code in either the vm or workstation will be mirrored
+ If you run a server, access it at >> %s
--------------------------------------------------------------------------------

`, devIP))

  return nil
}

// attachNetwork attaches the container to the host network
func (devConsole *processDevConsole) attachNetwork() error {

  // fetch the devIP
  devIP := devConsole.app.GlobalIPs["dev"]

  //
  if err := provider.AddIP(devIP); err != nil {
    return err
  }

  //
  if err := provider.AddNat(devIP, devConsole.localIP.String()); err != nil {
    return err
  }

  return nil
}

// detachNetwork detaches the container from the host network
func (devConsole *processDevConsole) detachNetwork() error {

  // fetch the devIP
  devIP := devConsole.app.GlobalIPs["dev"]

  //
  if err := provider.RemoveNat(devIP, devConsole.localIP.String()); err != nil {
    return err
  }

  //
  if err := provider.RemoveIP(devIP); err != nil {
    return err
  }

  return nil
}

// devIsUnused returns true if the dev isn't being used by any other session
func devIsUnused() bool {
  count, err := counter.Get(config.AppName() + "_dev")
  return count == 0 && err == nil
}

// isDevRunning returns true if a service is already running
func isDevRunning() bool {
  name := fmt.Sprintf("nanobox-%s-dev", config.AppName())

  container, err := docker.GetContainer(name)

  // if the container doesn't exist then just return false
  return err == nil && container.State.Status == "running"
}
