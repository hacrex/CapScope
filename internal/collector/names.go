package collector

import "strings"

const captainPrefix = "srv-captain--"

// AppName derives a friendly app name and replica id from a container.
// CapRover apps run as Swarm services named "srv-captain--<app>", and their
// tasks as "srv-captain--<app>.<slot>.<taskid>".
func AppName(name string, labels map[string]string) (app, replica string) {
	name = strings.TrimPrefix(name, "/")
	if svc := labels["com.docker.swarm.service.name"]; svc != "" {
		app = strings.TrimPrefix(svc, captainPrefix)
		if task := labels["com.docker.swarm.task.name"]; strings.HasPrefix(task, svc+".") {
			parts := strings.SplitN(strings.TrimPrefix(task, svc+"."), ".", 2)
			replica = parts[0]
		}
		return app, replica
	}
	if svc := labels["com.docker.compose.service"]; svc != "" {
		proj := labels["com.docker.compose.project"]
		if proj != "" {
			return proj + "/" + svc, labels["com.docker.compose.container-number"]
		}
		return svc, labels["com.docker.compose.container-number"]
	}
	if strings.HasPrefix(name, captainPrefix) {
		rest := strings.TrimPrefix(name, captainPrefix)
		parts := strings.Split(rest, ".")
		if len(parts) > 1 {
			return parts[0], parts[1]
		}
		return rest, ""
	}
	return name, ""
}
