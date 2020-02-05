// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/Azure/aks-engine/pkg/api"
	"github.com/pkg/errors"
)

var commonTemplateFiles = []string{agentOutputs, agentParams, masterOutputs, iaasOutputs, masterParams, windowsParams}
var kubernetesParamFiles = []string{armParameters, kubernetesParams, masterParams, agentParams, windowsParams}

var keyvaultSecretPathRe *regexp.Regexp

func init() {
	keyvaultSecretPathRe = regexp.MustCompile(`^(/subscriptions/\S+/resourceGroups/\S+/providers/Microsoft.KeyVault/vaults/\S+)/secrets/([^/\s]+)(/(\S+))?$`)
}

type paramsMap map[string]interface{}

// validateDistro checks if the requested orchestrator type is supported on the requested Linux distro.
func validateDistro(cs *api.ContainerService) bool {
	// Check Master distro
	if cs.Properties.MasterProfile != nil && cs.Properties.MasterProfile.Distro == api.RHEL &&
		(cs.Properties.OrchestratorProfile.OrchestratorType != api.SwarmMode) {
		log.Printf("Orchestrator type %s not suported on RHEL Master", cs.Properties.OrchestratorProfile.OrchestratorType)
		return false
	}
	// Check Agent distros
	for _, agentProfile := range cs.Properties.AgentPoolProfiles {
		if agentProfile.Distro == api.RHEL &&
			(cs.Properties.OrchestratorProfile.OrchestratorType != api.SwarmMode) {
			log.Printf("Orchestrator type %s not suported on RHEL Agent", cs.Properties.OrchestratorProfile.OrchestratorType)
			return false
		}
	}
	return true
}

// generateConsecutiveIPsList takes a starting IP address and returns a string slice of length "count" of subsequent, consecutive IP addresses
func generateConsecutiveIPsList(count int, firstAddr string) ([]string, error) {
	ipaddr := net.ParseIP(firstAddr).To4()
	if ipaddr == nil {
		return nil, errors.Errorf("IPAddr '%s' is an invalid IP address", firstAddr)
	}
	if int(ipaddr[3])+count >= 255 {
		return nil, errors.Errorf("IPAddr '%s' + %d will overflow the fourth octet", firstAddr, count)
	}
	ret := make([]string, count)
	for i := 0; i < count; i++ {
		nextAddress := fmt.Sprintf("%d.%d.%d.%d", ipaddr[0], ipaddr[1], ipaddr[2], ipaddr[3]+byte(i))
		ipaddr := net.ParseIP(nextAddress).To4()
		if ipaddr == nil {
			return nil, errors.Errorf("IPAddr '%s' is an invalid IP address", nextAddress)
		}
		ret[i] = nextAddress
	}
	return ret, nil
}

func addValue(m paramsMap, k string, v interface{}) {
	m[k] = paramsMap{
		"value": v,
	}
}

func addKeyvaultReference(m paramsMap, k string, vaultID, secretName, secretVersion string) {
	m[k] = paramsMap{
		"reference": &KeyVaultRef{
			KeyVault: KeyVaultID{
				ID: vaultID,
			},
			SecretName:    secretName,
			SecretVersion: secretVersion,
		},
	}
}

func addSecret(m paramsMap, k string, v interface{}, encode bool) {
	str, ok := v.(string)
	if !ok {
		addValue(m, k, v)
		return
	}
	parts := keyvaultSecretPathRe.FindStringSubmatch(str)
	if parts == nil || len(parts) != 5 {
		if encode {
			addValue(m, k, base64.StdEncoding.EncodeToString([]byte(str)))
		} else {
			addValue(m, k, str)
		}
		return
	}
	addKeyvaultReference(m, k, parts[1], parts[2], parts[4])
}

func makeMasterExtensionScriptCommands(cs *api.ContainerService) string {
	curlCaCertOpt := ""
	if cs.Properties.IsAzureStackCloud() {
		curlCaCertOpt = fmt.Sprintf("--cacert %s", AzureStackCaCertLocation)
	}
	return makeExtensionScriptCommands(cs.Properties.MasterProfile.PreprovisionExtension,
		curlCaCertOpt, cs.Properties.ExtensionProfiles)
}

func makeAgentExtensionScriptCommands(cs *api.ContainerService, profile *api.AgentPoolProfile) string {
	if profile.OSType == api.Windows {
		return makeWindowsExtensionScriptCommands(profile.PreprovisionExtension,
			cs.Properties.ExtensionProfiles)
	}
	curlCaCertOpt := ""
	if cs.Properties.IsAzureStackCloud() {
		curlCaCertOpt = fmt.Sprintf("--cacert %s", AzureStackCaCertLocation)
	}
	return makeExtensionScriptCommands(profile.PreprovisionExtension,
		curlCaCertOpt, cs.Properties.ExtensionProfiles)
}

func makeExtensionScriptCommands(extension *api.Extension, curlCaCertOpt string, extensionProfiles []*api.ExtensionProfile) string {
	var extensionProfile *api.ExtensionProfile
	for _, eP := range extensionProfiles {
		if strings.EqualFold(eP.Name, extension.Name) {
			extensionProfile = eP
			break
		}
	}

	if extensionProfile == nil {
		panic(fmt.Sprintf("%s extension referenced was not found in the extension profile", extension.Name))
	}

	extensionsParameterReference := fmt.Sprintf("parameters('%sParameters')", extensionProfile.Name)
	scriptURL := getExtensionURL(extensionProfile.RootURL, extensionProfile.Name, extensionProfile.Version, extensionProfile.Script, extensionProfile.URLQuery)
	scriptFilePath := fmt.Sprintf("/opt/azure/containers/extensions/%s/%s", extensionProfile.Name, extensionProfile.Script)
	return fmt.Sprintf("- sudo /usr/bin/curl --retry 5 --retry-delay 10 --retry-max-time 30 -o %s --create-dirs %s \"%s\" \n- sudo /bin/chmod 744 %s \n- sudo %s ',%s,' > /var/log/%s-output.log",
		scriptFilePath, curlCaCertOpt, scriptURL, scriptFilePath, scriptFilePath, extensionsParameterReference, extensionProfile.Name)
}

func makeWindowsExtensionScriptCommands(extension *api.Extension, extensionProfiles []*api.ExtensionProfile) string {
	var extensionProfile *api.ExtensionProfile
	for _, eP := range extensionProfiles {
		if strings.EqualFold(eP.Name, extension.Name) {
			extensionProfile = eP
			break
		}
	}

	if extensionProfile == nil {
		panic(fmt.Sprintf("%s extension referenced was not found in the extension profile", extension.Name))
	}

	scriptURL := getExtensionURL(extensionProfile.RootURL, extensionProfile.Name, extensionProfile.Version, extensionProfile.Script, extensionProfile.URLQuery)
	scriptFileDir := fmt.Sprintf("$env:SystemDrive:/AzureData/extensions/%s", extensionProfile.Name)
	scriptFilePath := fmt.Sprintf("%s/%s", scriptFileDir, extensionProfile.Script)
	return fmt.Sprintf("New-Item -ItemType Directory -Force -Path \"%s\" ; Invoke-WebRequest -Uri \"%s\" -OutFile \"%s\" ; powershell \"%s `\"',parameters('%sParameters'),'`\"\"\n", scriptFileDir, scriptURL, scriptFilePath, scriptFilePath, extensionProfile.Name)
}

func getVNETAddressPrefixes(properties *api.Properties) string {
	visitedSubnets := make(map[string]bool)
	var buf bytes.Buffer
	buf.WriteString(`"[variables('masterSubnet')]"`)
	visitedSubnets[properties.MasterProfile.Subnet] = true
	for _, profile := range properties.AgentPoolProfiles {
		if _, ok := visitedSubnets[profile.Subnet]; !ok {
			buf.WriteString(fmt.Sprintf(",\n            \"[variables('%sSubnet')]\"", profile.Name))
		}
	}
	return buf.String()
}

func getVNETSubnetDependencies(properties *api.Properties) string {
	agentString := `        "[concat('Microsoft.Network/networkSecurityGroups/', variables('%sNSGName'))]"`
	var buf bytes.Buffer
	for index, agentProfile := range properties.AgentPoolProfiles {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(fmt.Sprintf(agentString, agentProfile.Name))
	}
	return buf.String()
}

func getVNETSubnets(properties *api.Properties, addNSG bool) string {
	masterString := `{
            "name": "[variables('masterSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('masterSubnet')]"
            }
          }`
	agentString := `          {
            "name": "[variables('%sSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('%sSubnet')]"
            }
          }`
	agentStringNSG := `          {
            "name": "[variables('%sSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('%sSubnet')]",
              "networkSecurityGroup": {
                "id": "[resourceId('Microsoft.Network/networkSecurityGroups', variables('%sNSGName'))]"
              }
            }
          }`
	var buf bytes.Buffer
	buf.WriteString(masterString)
	for _, agentProfile := range properties.AgentPoolProfiles {
		buf.WriteString(",\n")
		if addNSG {
			buf.WriteString(fmt.Sprintf(agentStringNSG, agentProfile.Name, agentProfile.Name, agentProfile.Name))
		} else {
			buf.WriteString(fmt.Sprintf(agentString, agentProfile.Name, agentProfile.Name))
		}

	}
	return buf.String()
}

func getLBRule(name string, port int) string {
	return fmt.Sprintf(`	          {
            "name": "LBRule%d",
            "properties": {
              "backendAddressPool": {
                "id": "[concat(variables('%sLbID'), '/backendAddressPools/', variables('%sLbBackendPoolName'))]"
              },
              "backendPort": %d,
              "enableFloatingIP": false,
              "frontendIPConfiguration": {
                "id": "[variables('%sLbIPConfigID')]"
              },
              "frontendPort": %d,
              "idleTimeoutInMinutes": 5,
              "loadDistribution": "Default",
              "probe": {
                "id": "[concat(variables('%sLbID'),'/probes/tcp%dProbe')]"
              },
              "protocol": "Tcp"
            }
          }`, port, name, name, port, name, port, name, port)
}

func getLBRules(name string, ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getLBRule(name, port))
	}
	return buf.String()
}

func getProbe(port int) string {
	return fmt.Sprintf(`          {
            "name": "tcp%dProbe",
            "properties": {
              "intervalInSeconds": 5,
              "numberOfProbes": 2,
              "port": %d,
              "protocol": "Tcp"
            }
          }`, port, port)
}

func getProbes(ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getProbe(port))
	}
	return buf.String()
}

func getSecurityRule(port int, portIndex int) string {
	// BaseLBPriority specifies the base lb priority.
	BaseLBPriority := 200
	return fmt.Sprintf(`          {
            "name": "Allow_%d",
            "properties": {
              "access": "Allow",
              "description": "Allow traffic from the Internet to port %d",
              "destinationAddressPrefix": "*",
              "destinationPortRange": "%d",
              "direction": "Inbound",
              "priority": %d,
              "protocol": "*",
              "sourceAddressPrefix": "Internet",
              "sourcePortRange": "*"
            }
          }`, port, port, port, BaseLBPriority+portIndex)
}

func getDataDisks(a *api.AgentPoolProfile) string {
	if !a.HasDisks() {
		return ""
	}
	var buf bytes.Buffer
	buf.WriteString("\"dataDisks\": [\n")
	dataDisks := `            {
              "createOption": "Empty",
              "diskSizeGB": "%d",
              "lun": %d,
              "caching": "ReadOnly",
              "name": "[concat(variables('%sVMNamePrefix'), copyIndex(),'-datadisk%d')]",
              "vhd": {
                "uri": "[concat('http://',variables('storageAccountPrefixes')[mod(add(add(div(copyIndex(),variables('maxVMsPerStorageAccount')),variables('%sStorageAccountOffset')),variables('dataStorageAccountPrefixSeed')),variables('storageAccountPrefixesCount'))],variables('storageAccountPrefixes')[div(add(add(div(copyIndex(),variables('maxVMsPerStorageAccount')),variables('%sStorageAccountOffset')),variables('dataStorageAccountPrefixSeed')),variables('storageAccountPrefixesCount'))],variables('%sDataAccountName'),'.blob.core.windows.net/vhds/',variables('%sVMNamePrefix'),copyIndex(), '--datadisk%d.vhd')]"
              }
            }`
	managedDataDisks := `            {
              "diskSizeGB": "%d",
              "lun": %d,
              "caching": "ReadOnly",
              "createOption": "Empty"
            }`
	for i, diskSize := range a.DiskSizesGB {
		if i > 0 {
			buf.WriteString(",\n")
		}
		if a.StorageProfile == api.StorageAccount {
			buf.WriteString(fmt.Sprintf(dataDisks, diskSize, i, a.Name, i, a.Name, a.Name, a.Name, a.Name, i))
		} else if a.StorageProfile == api.ManagedDisks {
			buf.WriteString(fmt.Sprintf(managedDataDisks, diskSize, i))
		}
	}
	buf.WriteString("\n          ],")
	return buf.String()
}

func getSecurityRules(ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getSecurityRule(port, index))
	}
	return buf.String()
}

// getSingleLine returns the file as a single line
func (t *TemplateGenerator) getSingleLine(textFilename string, cs *api.ContainerService, profile interface{}) (string, error) {
	b, err := Asset(textFilename)
	if err != nil {
		return "", t.Translator.Errorf("yaml file %s does not exist", textFilename)
	}

	// use go templates to process the text filename
	templ := template.New("customdata template").Funcs(t.getTemplateFuncMap(cs))
	if _, err = templ.New(textFilename).Parse(string(b)); err != nil {
		return "", t.Translator.Errorf("error parsing file %s: %v", textFilename, err)
	}

	var buffer bytes.Buffer
	if err = templ.ExecuteTemplate(&buffer, textFilename, profile); err != nil {
		return "", t.Translator.Errorf("error executing template for file %s: %v", textFilename, err)
	}
	expandedTemplate := buffer.String()

	return expandedTemplate, nil
}

// getSingleLineForTemplate returns the file as a single line for embedding in an arm template
func (t *TemplateGenerator) getSingleLineForTemplate(textFilename string, cs *api.ContainerService, profile interface{}) (string, error) {
	expandedTemplate, err := t.getSingleLine(textFilename, cs, profile)
	if err != nil {
		return "", err
	}

	textStr := escapeSingleLine(expandedTemplate)

	return textStr, nil
}

func escapeSingleLine(escapedStr string) string {
	// template.JSEscapeString leaves undesirable chars that don't work with pretty print
	escapedStr = strings.Replace(escapedStr, "\\", "\\\\", -1)
	escapedStr = strings.Replace(escapedStr, "\r\n", "\\n", -1)
	escapedStr = strings.Replace(escapedStr, "\n", "\\n", -1)
	escapedStr = strings.Replace(escapedStr, "\"", "\\\"", -1)
	return escapedStr
}

// getBase64EncodedGzippedCustomScript will return a base64 of the CSE
func getBase64EncodedGzippedCustomScript(csFilename string, cs *api.ContainerService) string {
	b, err := Asset(csFilename)
	if err != nil {
		// this should never happen and this is a bug
		panic(fmt.Sprintf("BUG: %s", err.Error()))
	}
	// translate the parameters
	templ := template.New("ContainerService template").Funcs(getContainerServiceFuncMap(cs))
	_, err = templ.Parse(string(b))
	if err != nil {
		// this should never happen and this is a bug
		panic(fmt.Sprintf("BUG: %s", err.Error()))
	}
	var buffer bytes.Buffer
	templ.Execute(&buffer, cs)
	csStr := buffer.String()
	csStr = strings.Replace(csStr, "\r\n", "\n", -1)
	return getBase64EncodedGzippedCustomScriptFromStr(csStr)
}

func getStringFromBase64(str string) (string, error) {
	decodedBytes, err := base64.StdEncoding.DecodeString(str)
	return string(decodedBytes), err
}

// getBase64EncodedGzippedCustomScriptFromStr will return a base64-encoded string of the gzip'd source data
func getBase64EncodedGzippedCustomScriptFromStr(str string) string {
	var gzipB bytes.Buffer
	w := gzip.NewWriter(&gzipB)
	w.Write([]byte(str))
	w.Close()
	return base64.StdEncoding.EncodeToString(gzipB.Bytes())
}

func getAddonFuncMap(addon api.KubernetesAddon) template.FuncMap {
	return template.FuncMap{
		"ContainerImage": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].Image
		},

		"ContainerCPUReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPURequests
		},

		"ContainerCPULimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPULimits
		},

		"ContainerMemReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryRequests
		},

		"ContainerMemLimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryLimits
		},
		"ContainerConfig": func(name string) string {
			return addon.Config[name]
		},
	}
}

func getClusterAutoscalerAddonFuncMap(addon api.KubernetesAddon, cs *api.ContainerService) template.FuncMap {
	return template.FuncMap{
		"ContainerImage": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].Image
		},

		"ContainerCPUReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPURequests
		},

		"ContainerCPULimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPULimits
		},

		"ContainerMemReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryRequests
		},

		"ContainerMemLimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryLimits
		},
		"ContainerConfig": func(name string) string {
			return addon.Config[name]
		},
		"GetMode": func() string {
			return addon.Mode
		},
		"GetClusterAutoscalerNodesConfig": func() string {
			return api.GetClusterAutoscalerNodesConfig(addon, cs)
		},
		"GetVMType": func() string {
			if cs.Properties.AnyAgentUsesVirtualMachineScaleSets() {
				return base64.StdEncoding.EncodeToString([]byte("vmss"))
			}
			return base64.StdEncoding.EncodeToString([]byte("standard"))
		},
		"GetVolumeMounts": func() string {
			if cs.Properties.OrchestratorProfile.KubernetesConfig.UseManagedIdentity {
				return fmt.Sprintf("\n        - mountPath: /var/lib/waagent/\n          name: waagent\n          readOnly: true")
			}
			return ""
		},
		"GetVolumes": func() string {
			if cs.Properties.OrchestratorProfile.KubernetesConfig.UseManagedIdentity {
				return fmt.Sprintf("\n      - hostPath:\n          path: /var/lib/waagent/\n        name: waagent")
			}
			return ""
		},
		"GetHostNetwork": func() string {
			if cs.Properties.OrchestratorProfile.KubernetesConfig.UseManagedIdentity {
				return fmt.Sprintf("\n      hostNetwork: true")
			}
			return ""
		},
		"GetCloud": func() string {
			cloudSpecConfig := cs.GetCloudSpecConfig()
			return cloudSpecConfig.CloudName
		},
		"UseManagedIdentity": func() string {
			if cs.Properties.OrchestratorProfile.KubernetesConfig.UseManagedIdentity {
				return "true"
			}
			return "false"
		},
	}
}

func getContainerAddonsString(cs *api.ContainerService, sourcePath string) string {
	properties := cs.Properties
	var result string
	settingsMap := kubernetesContainerAddonSettingsInit(properties)

	var addonNames []string

	for addonName := range settingsMap {
		addonNames = append(addonNames, addonName)
	}

	sort.Strings(addonNames)

	for _, addonName := range addonNames {
		setting := settingsMap[addonName]
		if cs.Properties.OrchestratorProfile.KubernetesConfig.IsAddonEnabled(addonName) {
			var input string
			if setting.base64Data != "" {
				var err error
				input, err = getStringFromBase64(setting.base64Data)
				if err != nil {
					return ""
				}
			} else {
				orchProfile := properties.OrchestratorProfile
				versions := strings.Split(orchProfile.OrchestratorVersion, ".")
				addon := orchProfile.KubernetesConfig.GetAddonByName(addonName)
				var templ *template.Template
				switch addonName {
				case "cluster-autoscaler":
					templ = template.New("addon resolver template").Funcs(getClusterAutoscalerAddonFuncMap(addon, cs))
				default:
					templ = template.New("addon resolver template").Funcs(getAddonFuncMap(addon))
				}
				addonFile := getCustomDataFilePath(setting.sourceFile, sourcePath, versions[0]+"."+versions[1])
				addonFileBytes, err := Asset(addonFile)
				if err != nil {
					return ""
				}
				_, err = templ.Parse(string(addonFileBytes))
				if err != nil {
					return ""
				}
				var buffer bytes.Buffer
				templ.Execute(&buffer, addon)
				input = buffer.String()
			}
			result += getAddonString(input, "/etc/kubernetes/addons", setting.destinationFile)
		}
	}
	return result
}

func buildYamlFileWithWriteFiles(files []string, cs *api.ContainerService) string {
	clusterYamlFile := `#cloud-config

write_files:
%s
`
	writeFileBlock := ` -  encoding: gzip
    content: !!binary |
        %s
    path: /opt/azure/containers/%s
    permissions: "0744"
`

	filelines := ""
	for _, file := range files {
		b64GzipString := getBase64EncodedGzippedCustomScript(file, cs)
		fileNoPath := strings.TrimPrefix(file, "swarm/")
		filelines += fmt.Sprintf(writeFileBlock, b64GzipString, fileNoPath)
	}
	return fmt.Sprintf(clusterYamlFile, filelines)
}

func getKubernetesSubnets(properties *api.Properties) string {
	subnetString := `{
            "name": "podCIDR%d",
            "properties": {
              "addressPrefix": "10.244.%d.0/24",
              "networkSecurityGroup": {
                "id": "[variables('nsgID')]"
              },
              "routeTable": {
                "id": "[variables('routeTableID')]"
              }
            }
          }`
	var buf bytes.Buffer

	cidrIndex := getKubernetesPodStartIndex(properties)
	for _, agentProfile := range properties.AgentPoolProfiles {
		if agentProfile.OSType == api.Windows {
			for i := 0; i < agentProfile.Count; i++ {
				buf.WriteString(",\n")
				buf.WriteString(fmt.Sprintf(subnetString, cidrIndex, cidrIndex))
				cidrIndex++
			}
		}
	}
	return buf.String()
}

func getKubernetesPodStartIndex(properties *api.Properties) int {
	nodeCount := 0
	nodeCount += properties.MasterProfile.Count
	for _, agentProfile := range properties.AgentPoolProfiles {
		if agentProfile.OSType != api.Windows {
			nodeCount += agentProfile.Count
		}
	}

	return nodeCount + 1
}

func getMasterLinkedTemplateText(orchestratorType string, extensionProfile *api.ExtensionProfile, singleOrAll string) (string, error) {
	extTargetVMNamePrefix := "variables('masterVMNamePrefix')"

	loopCount := "[variables('masterCount')]"
	loopOffset := ""
	if orchestratorType == api.Kubernetes {
		// Due to upgrade k8s sometimes needs to install just some of the nodes.
		loopCount = "[sub(variables('masterCount'), variables('masterOffset'))]"
		loopOffset = "variables('masterOffset')"
	}

	if strings.EqualFold(singleOrAll, "single") {
		loopCount = "1"
	}
	return internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount,
		loopOffset, extensionProfile)
}

func getAgentPoolLinkedTemplateText(agentPoolProfile *api.AgentPoolProfile, orchestratorType string, extensionProfile *api.ExtensionProfile, singleOrAll string) (string, error) {
	extTargetVMNamePrefix := fmt.Sprintf("variables('%sVMNamePrefix')", agentPoolProfile.Name)
	loopCount := fmt.Sprintf("[variables('%sCount'))]", agentPoolProfile.Name)
	loopOffset := ""

	// Availability sets can have an offset since we don't redeploy vms.
	// So we don't want to rerun these extensions in scale up scenarios.
	if agentPoolProfile.IsAvailabilitySets() {
		loopCount = fmt.Sprintf("[sub(variables('%sCount'), variables('%sOffset'))]",
			agentPoolProfile.Name, agentPoolProfile.Name)
		loopOffset = fmt.Sprintf("variables('%sOffset')", agentPoolProfile.Name)
	}

	if strings.EqualFold(singleOrAll, "single") {
		loopCount = "1"
	}

	return internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount,
		loopOffset, extensionProfile)
}

func internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount, loopOffset string, extensionProfile *api.ExtensionProfile) (string, error) {
	dta, e := getLinkedTemplateTextForURL(extensionProfile.RootURL, orchestratorType, extensionProfile.Name, extensionProfile.Version, extensionProfile.URLQuery)
	if e != nil {
		return "", e
	}
	if strings.Contains(extTargetVMNamePrefix, "master") {
		dta = strings.Replace(dta, "EXTENSION_TARGET_VM_TYPE", "master", -1)
	} else {
		dta = strings.Replace(dta, "EXTENSION_TARGET_VM_TYPE", "agent", -1)
	}
	extensionsParameterReference := fmt.Sprintf("[parameters('%sParameters')]", extensionProfile.Name)
	dta = strings.Replace(dta, "EXTENSION_PARAMETERS_REPLACE", extensionsParameterReference, -1)
	dta = strings.Replace(dta, "EXTENSION_URL_REPLACE", extensionProfile.RootURL, -1)
	dta = strings.Replace(dta, "EXTENSION_TARGET_VM_NAME_PREFIX", extTargetVMNamePrefix, -1)
	if _, err := strconv.Atoi(loopCount); err == nil {
		dta = strings.Replace(dta, "\"EXTENSION_LOOP_COUNT\"", loopCount, -1)
	} else {
		dta = strings.Replace(dta, "EXTENSION_LOOP_COUNT", loopCount, -1)
	}

	dta = strings.Replace(dta, "EXTENSION_LOOP_OFFSET", loopOffset, -1)
	return dta, nil
}

func validateProfileOptedForExtension(extensionName string, profileExtensions []api.Extension) (bool, string) {
	for _, extension := range profileExtensions {
		if extensionName == extension.Name {
			return true, extension.SingleOrAll
		}
	}
	return false, ""
}

// getLinkedTemplateTextForURL returns the string data from
// template-link.json in the following directory:
// extensionsRootURL/extensions/extensionName/version
// It returns an error if the extension cannot be found
// or loaded.  getLinkedTemplateTextForURL provides the ability
// to pass a root extensions url for testing
func getLinkedTemplateTextForURL(rootURL, orchestrator, extensionName, version, query string) (string, error) {
	supportsExtension, err := orchestratorSupportsExtension(rootURL, orchestrator, extensionName, version, query)
	if !supportsExtension {
		return "", errors.Wrap(err, "Extension not supported for orchestrator")
	}

	templateLinkBytes, err := getExtensionResource(rootURL, extensionName, version, "template-link.json", query)
	if err != nil {
		return "", err
	}

	return string(templateLinkBytes), nil
}

func orchestratorSupportsExtension(rootURL, orchestrator, extensionName, version, query string) (bool, error) {
	orchestratorBytes, err := getExtensionResource(rootURL, extensionName, version, "supported-orchestrators.json", query)
	if err != nil {
		return false, err
	}

	var supportedOrchestrators []string
	err = json.Unmarshal(orchestratorBytes, &supportedOrchestrators)
	if err != nil {
		return false, errors.Errorf("Unable to parse supported-orchestrators.json for Extension %s Version %s", extensionName, version)
	}

	if !stringInSlice(orchestrator, supportedOrchestrators) {
		return false, errors.Errorf("Orchestrator: %s not in list of supported orchestrators for Extension: %s Version %s", orchestrator, extensionName, version)
	}

	return true, nil
}

func getExtensionResource(rootURL, extensionName, version, fileName, query string) ([]byte, error) {
	requestURL := getExtensionURL(rootURL, extensionName, version, fileName, query)

	res, err := http.Get(requestURL)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to GET extension resource for extension: %s with version %s with filename %s at URL: %s", extensionName, version, fileName, requestURL)
	}

	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.Errorf("Unable to GET extension resource for extension: %s with version %s with filename %s at URL: %s StatusCode: %s: Status: %s", extensionName, version, fileName, requestURL, strconv.Itoa(res.StatusCode), res.Status)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to GET extension resource for extension: %s with version %s  with filename %s at URL: %s", extensionName, version, fileName, requestURL)
	}

	return body, nil
}

func getExtensionURL(rootURL, extensionName, version, fileName, query string) string {
	extensionsDir := "extensions"
	url := rootURL + extensionsDir + "/" + extensionName + "/" + version + "/" + fileName
	if query != "" {
		url += "?" + query
	}
	return url
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func wrapAsVariableObject(o, v string) string {
	return fmt.Sprintf("',variables('%s').%s,'", o, v)
}

func getSSHPublicKeysPowerShell(linuxProfile *api.LinuxProfile) string {
	str := ""
	if linuxProfile != nil {
		lastItem := len(linuxProfile.SSH.PublicKeys) - 1
		for i, publicKey := range linuxProfile.SSH.PublicKeys {
			str += `"` + strings.TrimSpace(publicKey.KeyData) + `"`
			if i < lastItem {
				str += ", "
			}
		}
	}
	return str
}

func getWindowsMasterSubnetARMParam(masterProfile *api.MasterProfile) string {
	if masterProfile != nil && masterProfile.IsCustomVNET() {
		return fmt.Sprintf("',parameters('vnetCidr'),'")
	}
	return fmt.Sprintf("',parameters('masterSubnet'),'")
}

// IsNvidiaEnabledSKU determines if an VM SKU has nvidia driver support
func IsNvidiaEnabledSKU(vmSize string) bool {
	/* If a new GPU sku becomes available, add a key to this map, but only if you have a confirmation
	   that we have an agreement with NVIDIA for this specific gpu.
	*/
	dm := map[string]bool{
		// K80
		"Standard_NC6":   true,
		"Standard_NC12":  true,
		"Standard_NC24":  true,
		"Standard_NC24r": true,
		// M60
		"Standard_NV6":   true,
		"Standard_NV12":  true,
		"Standard_NV24":  true,
		"Standard_NV24r": true,
		// P40
		"Standard_ND6s":   true,
		"Standard_ND12s":  true,
		"Standard_ND24s":  true,
		"Standard_ND24rs": true,
		// P100
		"Standard_NC6s_v2":   true,
		"Standard_NC12s_v2":  true,
		"Standard_NC24s_v2":  true,
		"Standard_NC24rs_v2": true,
		// V100
		"Standard_NC6s_v3":   true,
		"Standard_NC12s_v3":  true,
		"Standard_NC24s_v3":  true,
		"Standard_NC24rs_v3": true,
		"Standard_ND40s_v3":  true,
		"Standard_ND40rs_v2": true,
	}
	// Trim the optional _Promo suffix.
	vmSize = strings.TrimSuffix(vmSize, "_Promo")
	if _, ok := dm[vmSize]; ok {
		return dm[vmSize]
	}

	return false
}

// IsSgxEnabledSKU determines if an VM SKU has SGX driver support
func IsSgxEnabledSKU(vmSize string) bool {
	switch vmSize {
	case "Standard_DC2s", "Standard_DC4s":
		return true
	}
	return false
}

// GetCloudTargetEnv determines and returns whether the region is a sovereign cloud which
// have their own data compliance regulations (China/Germany/USGov) or standard
// Azure public cloud
func GetCloudTargetEnv(location string) string {
	loc := strings.ToLower(strings.Join(strings.Fields(location), ""))
	switch {
	case loc == "chinaeast" || loc == "chinanorth" || loc == "chinaeast2" || loc == "chinanorth2":
		return "AzureChinaCloud"
	case loc == "germanynortheast" || loc == "germanycentral":
		return "AzureGermanCloud"
	case strings.HasPrefix(loc, "usgov") || strings.HasPrefix(loc, "usdod"):
		return "AzureUSGovernmentCloud"
	default:
		return "AzurePublicCloud"
	}
}

// IsKubernetesVersionGe returns true if actualVersion is greater than or equal to version
func IsKubernetesVersionGe(actualVersion, version string) bool {
	v1, _ := semver.Make(actualVersion)
	v2, _ := semver.Make(version)
	return v1.GE(v2)
}
