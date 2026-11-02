// Cost recovery trace: replays how a task's spend is resolved so the admin can
// inspect the exact endpoint, payload and response behind every figure.
//
// The chain mirrors what the generation core and the login backfill do:
// provider /estimate (Higgsfield) → local calculator → usage block of the
// submit response. The trace answers the "why is this cost wrong?" question
// with the real HTTP exchange instead of a summary.
package agency

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"synapta/internal/modules/credential"
)

// CostRecoveryRow is the spend of a task as stored in generation_logs.
type CostRecoveryRow struct {
	EstimatedCost float64 `json:"estimated_cost"`
	CostCredits   float64 `json:"cost_credits"`
	CostSource    string  `json:"cost_source"`
}

// CostRecoveryStep is one attempt of the resolution chain (payload, provider
// estimate, stored estimate, local calculator, provider usage) with the HTTP
// exchange it produced, when it made one.
type CostRecoveryStep struct {
	Key          string  `json:"key"`
	Method       string  `json:"method,omitempty"`
	Endpoint     string  `json:"endpoint,omitempty"`
	RequestBody  string  `json:"request_body,omitempty"`
	StatusCode   int     `json:"status_code,omitempty"`
	ResponseBody string  `json:"response_body,omitempty"`
	Credits      float64 `json:"credits,omitempty"`
	USD          float64 `json:"usd,omitempty"`
	Found        bool    `json:"found"`
	Note         string  `json:"note,omitempty"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
}

// CostRecoveryTrace explains and (when allowed) repairs the spend of one task.
type CostRecoveryTrace struct {
	TaskID    string             `json:"task_id"`
	ModelName string             `json:"model_name"`
	Provider  string             `json:"provider"`
	Generator string             `json:"generator"`
	Before    CostRecoveryRow    `json:"before"`
	After     CostRecoveryRow    `json:"after"`
	Applied   bool               `json:"applied"`
	Decision  string             `json:"decision"`
	Reason    string             `json:"reason"`
	Steps     []CostRecoveryStep `json:"steps"`
}

// recoveryShape is the request shape the local calculators need, rebuilt from
// the stored client payload (the provider body is already flattened).
type recoveryShape struct {
	Model      string   `json:"model"`
	Duration   int      `json:"duration"`
	Resolution string   `json:"resolution"`
	Ratio      string   `json:"ratio"`
	Quantity   int      `json:"quantity"`
	Content    []string `json:"content_types"`
}

// newRecoveryShape rebuilds the calculator input from request_payload, falling
// back to the submitted provider body and the video metadata columns.
func newRecoveryShape(row *GenerationLog, payload map[string]interface{}) recoveryShape {
	shape := recoveryShape{Model: row.ModelName}
	var client backfillPayload
	if err := json.Unmarshal([]byte(row.Request), &client); err == nil {
		shape.Duration = int(client.Duration)
		shape.Resolution = client.Resolution
		shape.Ratio = client.Ratio
		shape.Quantity = client.Quantity
		for _, item := range client.Content {
			if item.Type != "" {
				shape.Content = append(shape.Content, item.Type)
			}
		}
	}
	if shape.Duration == 0 {
		shape.Duration = int(HiggsfieldNumber(payload["duration"]))
	}
	if shape.Resolution == "" {
		if s, ok := payload["resolution"].(string); ok {
			shape.Resolution = s
		}
	}
	if shape.Ratio == "" {
		if s, ok := payload["aspect_ratio"].(string); ok {
			shape.Ratio = s
		}
	}
	if shape.Quantity == 0 {
		shape.Quantity = int(HiggsfieldNumber(payload["num_images"]))
	}
	if shape.Duration == 0 {
		shape.Duration = row.VideoDuration
	}
	if shape.Resolution == "" {
		shape.Resolution = row.VideoResolution
	}
	if shape.Ratio == "" {
		shape.Ratio = row.VideoRatio
	}
	return shape
}

// generatorRequest converts the shape into the calculator input.
func (sh recoveryShape) generatorRequest() *GeneratorRequest {
	req := &GeneratorRequest{
		Model:      sh.Model,
		Duration:   sh.Duration,
		Resolution: sh.Resolution,
		Ratio:      sh.Ratio,
		Quantity:   sh.Quantity,
	}
	for _, t := range sh.Content {
		req.Content = append(req.Content, ContentItem{Type: t})
	}
	return req
}

// RecoverTaskCost resolves the spend of one task: it replays the provider
// estimate with the exact submitted payload, evaluates the local calculator and
// the provider usage block, then repairs generation_logs when the stored value
// is missing or provider-reported. Returns nil when the task has no log.
func (s *Core) RecoverTaskCost(taskID string) (*CostRecoveryTrace, error) {
	if s.logStore == nil || taskID == "" {
		return nil, nil
	}
	row, err := s.logStore.GetByTaskID(taskID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}

	trace := &CostRecoveryTrace{
		TaskID:    taskID,
		ModelName: row.ModelName,
		Before: CostRecoveryRow{
			EstimatedCost: row.EstimatedCost,
			CostCredits:   row.CostCredits,
			CostSource:    row.CostSource,
		},
	}
	m := LookupModel(row.ModelName, "")
	if m != nil {
		trace.Provider = string(m.CredentialProvider)
		trace.Generator = m.Generator
	}

	// Exact payload submitted to the provider: the estimate must price the
	// same body, not a rebuilt one.
	var genTrace *ServerCommunication
	if s.commStore != nil {
		genTrace, _ = s.commStore.GetByTaskIDAndPhase(taskID, "generate")
	}
	payloadBody := ""
	if genTrace != nil {
		payloadBody = genTrace.RequestBody
	}
	if payloadBody == "" {
		payloadBody = row.Request
	}
	payload := map[string]interface{}{}
	if payloadBody != "" {
		_ = json.Unmarshal([]byte(payloadBody), &payload)
	}
	trace.Steps = append(trace.Steps, CostRecoveryStep{
		Key:         "payload",
		RequestBody: prettyJSON(payloadBody),
		Found:       len(payload) > 0,
		Note:        "Payload exacto guardado del submit: es el cuerpo que se reenvía a la ruta de estimación.",
	})

	// Shape fed to the local calculator (the provider payload has no content).
	shape := newRecoveryShape(row, payload)
	shapeJSON, _ := json.Marshal(shape)
	trace.Steps = append(trace.Steps, CostRecoveryStep{
		Key:          "shape",
		ResponseBody: prettyJSON(string(shapeJSON)),
		Found:        true,
		Note:         "Forma reconstruida desde request_payload para la calculadora local (sin duración de entrada: no se guarda).",
	})

	// Provider route: per-tenant credential first, catalog route as fallback.
	var creds *credential.Resolve
	baseURL, endpoint := "", ""
	if m != nil && s.credStore != nil {
		if resolved, cerr := s.credStore.Get(string(m.CredentialProvider)); cerr == nil && resolved != nil {
			creds = resolved
			baseURL, endpoint = resolveRoute(m, creds)
		}
	}
	// The submitted route wins over the catalog one: i2v/reference reroutes
	// change the endpoint, and the estimate must price the payload that was
	// actually sent.
	if genTrace != nil && genTrace.Endpoint != "" {
		if u, perr := url.Parse(genTrace.Endpoint); perr == nil && u.Path != "" {
			if baseURL == "" {
				baseURL = u.Scheme + "://" + u.Host
			}
			endpoint = strings.TrimPrefix(u.Path, "/")
		}
	}

	canEstimate := trace.Provider == string(credential.ProviderHiggsfield) &&
		creds != nil && creds.AuthKey() != "" && len(payload) > 0 && baseURL != "" && endpoint != ""

	var est EstimateTrace
	estStep := CostRecoveryStep{Key: "estimate"}
	if canEstimate {
		est = TraceHiggsfieldEstimate(nil, baseURL, endpoint, payload, creds.AuthKey())
		estStep.Method = "POST"
		estStep.Endpoint = est.URL
		estStep.RequestBody = prettyJSON(est.RequestBody)
		estStep.ResponseBody = prettyJSON(est.ResponseBody)
		estStep.StatusCode = est.StatusCode
		estStep.Credits = est.Credits
		estStep.USD = est.USD
		estStep.Found = est.OK
		estStep.DurationMs = est.DurationMs
		if est.OK {
			estStep.Note = "Estimación oficial del proveedor aceptada."
		} else {
			estStep.Note = "La ruta /estimate no devolvió credits/usd utilizables."
			if est.Error != "" {
				estStep.Note += " " + est.Error
			}
		}
	} else {
		switch {
		case trace.Provider == "":
			estStep.Note = "El modelo no está en el catálogo: no hay proveedor ni ruta de estimación."
		case trace.Provider != string(credential.ProviderHiggsfield):
			estStep.Note = "El proveedor " + trace.Provider + " no expone /estimate: el gasto se resuelve con calculadora o usage."
		case len(payload) == 0:
			estStep.Note = "No hay payload guardado para reenviar."
		case creds == nil:
			estStep.Note = "El tenant no tiene credencial guardada para " + trace.Provider + "."
		case creds.AuthKey() == "":
			estStep.Note = "La credencial no tiene un secreto utilizable."
		default:
			estStep.Note = "No se pudo resolver la base URL o el endpoint del modelo."
		}
	}
	trace.Steps = append(trace.Steps, estStep)

	// What the original submit recorded (this is what the table shows today).
	if s.commStore != nil {
		if stored, serr := s.commStore.GetByTaskIDAndPhase(taskID, "estimate"); serr == nil && stored != nil {
			credits, usd := parseEstimateValues(stored.ResponseBody)
			trace.Steps = append(trace.Steps, CostRecoveryStep{
				Key:          "stored-estimate",
				Method:       stored.Method,
				Endpoint:     stored.Endpoint,
				RequestBody:  prettyJSON(stored.RequestBody),
				StatusCode:   stored.StatusCode,
				ResponseBody: prettyJSON(stored.ResponseBody),
				Credits:      credits,
				USD:          usd,
				Found:        credits > 0 || usd > 0,
				Note:         "Estimación registrada en el submit original: es el valor que hoy muestra la tabla.",
			})
		}
	}

	// Provider's own usage analytics: the numbers behind its billing screen.
	// Useful to confirm whether the provider actually charged the task (failed
	// and canceled requests are refunded) before trusting a local estimate.
	provUsageStep := CostRecoveryStep{Key: "provider-usage"}
	exactUsageUSD, exactUsageOK := 0.0, false
	canUsage := trace.Provider == string(credential.ProviderHiggsfield) &&
		creds != nil && creds.AuthKey() != "" && baseURL != ""
	if canUsage {
		var submittedAt time.Time
		if genTrace != nil {
			submittedAt = genTrace.StartedAt
		}
		usage := TraceHiggsfieldUsage(nil, baseURL, creds.AuthKey(), submittedAt, time.Time{})
		provUsageStep.Method = usage.Method
		provUsageStep.Endpoint = usage.URL
		provUsageStep.StatusCode = usage.StatusCode
		provUsageStep.ResponseBody = prettyJSON(usage.ResponseBody)
		provUsageStep.USD = usage.Total
		provUsageStep.Found = usage.OK && usage.Lines > 0
		provUsageStep.DurationMs = usage.DurationMs
		switch {
		case !usage.OK:
			provUsageStep.Note = "La ruta /models/usage no respondió con datos utilizables."
			if usage.Error != "" {
				provUsageStep.Note += " " + usage.Error
			}
		case usage.Lines == 0:
			provUsageStep.Note = "El proveedor no reporta consumo en la ventana del submit: la generación pudo fallar, moderarse o reembolsarse."
		default:
			provUsageStep.Note = fmt.Sprintf(
				"El proveedor reporta $%.4f en %d líneas de esa ventana (analítica del panel, no factura: puede incluir otras generaciones y demorar).",
				usage.Total, usage.Lines,
			)
		}
		// Una sola generación facturada para este modelo y esta API key: el
		// monto del panel es el de esta tarea, no un agregado de la ventana.
		exactUsageUSD, exactUsageOK = usage.ExactCost(endpoint, keyIDOf(creds))
		if exactUsageOK {
			provUsageStep.Note += fmt.Sprintf(" Coincide con esta generación: $%.4f por 1 unidad.", exactUsageUSD)
		}
	} else {
		switch {
		case trace.Provider == "":
			provUsageStep.Note = "El modelo no está en el catálogo: no hay proveedor al que consultar consumo."
		case trace.Provider != string(credential.ProviderHiggsfield):
			provUsageStep.Note = "El proveedor " + trace.Provider + " no expone una ruta de consumo equivalente."
		case creds == nil:
			provUsageStep.Note = "El tenant no tiene credencial guardada para " + trace.Provider + "."
		default:
			provUsageStep.Note = "No se pudo resolver la base URL del proveedor para consultar consumo."
		}
	}
	trace.Steps = append(trace.Steps, provUsageStep)

	// Local calculator (Seedance/Seedream families).
	calcUSD, calcName := 0.0, ""
	if calc := s.pickCalculator(row.ModelName); calc != nil {
		calcName = calc.Name()
		calcUSD = calc.CalculateEstimated(shape.generatorRequest())
	}
	calcBody, _ := json.Marshal(map[string]interface{}{
		"calculator": calcName,
		"usd":        calcUSD,
		"input":      shape,
	})
	calcStep := CostRecoveryStep{
		Key:          "calculator",
		Endpoint:     "local:" + calcName,
		ResponseBody: prettyJSON(string(calcBody)),
		USD:          calcUSD,
		Found:        calcUSD > 0,
	}
	switch {
	case calcName == "":
		calcStep.Note = "Ninguna calculadora hace Match con el modelo."
	case calcName == "default":
		calcStep.Note = "La calculadora default no tiene precios: siempre devuelve 0."
	default:
		calcStep.Note = "Precio calculado en local con la tarifa de " + calcName + "."
	}
	trace.Steps = append(trace.Steps, calcStep)

	// Provider usage block of the submit response (sync generators bill inline).
	usageUSD, usageBody := 0.0, ""
	if genTrace != nil && genTrace.ResponseBody != "" {
		var raw map[string]interface{}
		if json.Unmarshal([]byte(genTrace.ResponseBody), &raw) == nil {
			if usage, ok := raw["usage"].(map[string]interface{}); ok {
				usageUSD = HiggsfieldNumber(usage["cost"])
				if b, err := json.Marshal(usage); err == nil {
					usageBody = string(b)
				}
			}
		}
	}
	usageStep := CostRecoveryStep{
		Key:          "usage",
		Endpoint:     "stored generate response",
		ResponseBody: prettyJSON(usageBody),
		USD:          usageUSD,
		Found:        usageUSD > 0,
		Note:         "Bloque usage de la respuesta del submit (usage.cost).",
	}
	if usageUSD == 0 {
		usageStep.Note = "La respuesta del submit no trae usage.cost utilizable."
	}
	trace.Steps = append(trace.Steps, usageStep)

	// Resolution: provider estimate wins, then the local calculator, then the
	// inline usage block — the same precedence as the generation core.
	usd, credits, source, reason := 0.0, 0.0, "", ""
	switch {
	case est.OK && (est.USD > 0 || est.Credits > 0):
		usd, credits, source = est.USD, est.Credits, "provider_estimate"
		if usd == 0 && calcUSD > 0 {
			usd = calcUSD
			reason = fmt.Sprintf("El proveedor informó %.3f créditos sin USD: el monto en dólares se completa con la calculadora %s.", credits, calcName)
		} else {
			reason = fmt.Sprintf("Estimación del proveedor aceptada: %.3f créditos ≈ $%.4f.", credits, usd)
		}
	case exactUsageOK && exactUsageUSD > 0:
		usd, source = exactUsageUSD, "provider_usage"
		reason = fmt.Sprintf("El proveedor factura $%.4f por esta generación en su panel de consumo: se usa ese monto en lugar del calculado.", exactUsageUSD)
	case calcUSD > 0:
		usd, source = calcUSD, "calculator"
		reason = fmt.Sprintf("Sin estimación del proveedor: se usa la calculadora %s ($%.4f).", calcName, calcUSD)
	case usageUSD > 0:
		usd, source = usageUSD, "api_response"
		reason = fmt.Sprintf("Sin estimación ni calculadora: se recupera usage.cost del submit ($%.4f).", usageUSD)
	default:
		reason = "Ninguna fuente devolvió un monto: no hay nada que recuperar para esta tarea."
	}
	trace.Decision = source
	if trace.Decision == "" {
		trace.Decision = "none"
	}
	trace.Reason = reason

	// Provider-reported rows are refreshed with the live estimate (the stored
	// figure may come from a failed/stale call); local costs only fill empty
	// rows, and refunds are never touched.
	apply := source != "" && row.CostSource != "provider_refund" &&
		(row.EstimatedCost == 0 || source == "provider_estimate" || source == "provider_usage")
	if !apply {
		trace.After = trace.Before
		if source != "" {
			trace.Reason = reason + " No se sobrescribe: la fila ya tiene un gasto local calculado."
		}
		return trace, nil
	}
	applied, err := s.logStore.ApplyProviderCostByID(row.ID, usd, credits, source)
	if err != nil {
		return nil, err
	}
	trace.Applied = applied > 0
	if applied == 0 {
		trace.Reason = reason + " No se escribió nada: la fila es un reembolso del proveedor."
	}
	if after, cerr := s.logStore.GetCostByID(row.ID); cerr == nil {
		trace.After = after
	} else {
		trace.After = CostRecoveryRow{EstimatedCost: usd, CostCredits: credits, CostSource: source}
	}
	s.recordRecoveredEstimate(row, genTrace, estStep.Endpoint, payloadBody, usd, credits, source)
	return trace, nil
}

// keyIDOf returns the provider key id (the part before the secret) so usage
// analytics lines can be filtered by the tenant's credential.
func keyIDOf(creds *credential.Resolve) string {
	if creds == nil {
		return ""
	}
	key := creds.AuthKey()
	if i := strings.Index(key, ":"); i > 0 {
		return key[:i]
	}
	return key
}

// recordRecoveredEstimate stores the recovered spend as an estimate-phase trace
// so the logs table reflects the repaired value and keeps the audit trail.
func (s *Core) recordRecoveredEstimate(
	row *GenerationLog,
	genTrace *ServerCommunication,
	endpoint, requestBody string,
	usd, credits float64,
	source string,
) {
	if s.commStore == nil || row == nil {
		return
	}
	if endpoint == "" {
		endpoint = "local"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"credits":        credits,
		"usd":            usd,
		"source":         source,
		"transaction_id": row.ProviderTransactionID,
		"recovered":      true,
	})
	comm := &ServerCommunication{
		TaskID:       row.TaskID,
		ModelName:    row.ModelName,
		Endpoint:     endpoint,
		Method:       "POST",
		Phase:        "estimate",
		StartedAt:    time.Now(),
		FinishedAt:   time.Now(),
		RequestBody:  requestBody,
		ResponseBody: string(body),
		StatusCode:   200,
		TenantSlug:   s.slug,
	}
	if genTrace != nil {
		comm.UserID = genTrace.UserID
		comm.Username = genTrace.Username
		comm.CredentialProvider = genTrace.CredentialProvider
		comm.APIKeyMask = genTrace.APIKeyMask
		comm.AccessKeyMask = genTrace.AccessKeyMask
		comm.SecretKeyMask = genTrace.SecretKeyMask
		comm.AuthType = genTrace.AuthType
	}
	if err := s.commStore.Create(comm); err != nil {
		log.Printf("[cost-recovery] estimate trace %s: %v", row.TaskID, err)
	}
}

// parseEstimateValues reads the credits/usd pair of a stored estimate body.
func parseEstimateValues(body string) (credits, usd float64) {
	if body == "" {
		return 0, 0
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return 0, 0
	}
	return HiggsfieldNumber(raw["credits"]), HiggsfieldNumber(raw["usd"])
}

// prettyJSON returns an indented JSON body, or the raw text when it does not
// parse, so the admin dialog renders payloads readably.
func prettyJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}
