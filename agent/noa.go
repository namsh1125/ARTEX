package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa는 norma v0.4.0에서 도입한 모델 기반 컨텍스트 압축입니다. 플랫폼 실험 기능으로
// 시스템 설정에서 켜고 끌 수 있습니다. 내장 compaction과 상호 배타적이며 noaadapter.Enable이 유일한 진입점입니다.
// 컨텍스트 관리자(Compactor), Compress 도구, 세 구간의 상주 프롬프트를 한 번에 연결합니다.
// Enable을 호출하지 않으면 꺼진 상태로 내장 compaction이 정상 작동합니다. 각 Agent의 noaEnabledFn이
// 실행마다 한 번 설정을 읽으므로 전환은 이후 시작되는 실행에만 적용되고 Agent를 재생성할 필요가 없습니다.

// enableNoa는 설정이 활성화되었다고 보고할 때 noa를 opts에 연결합니다. archiveRoot는 압축 원문의 영구 저장 기준 경로입니다.
// 전역 workDir를 사용해 모든 Agent가 <workDir>/noa 아래에 저장하며 작업/의도 디렉터리로 분산하지 않습니다. sessionID가
// 하위 아카이브 디렉터리 이름입니다(전역 고유하므로 같은 기준 경로에서 충돌하지 않음).
//
// noa는 실험 기능이므로 연결 실패가 실제 작업을 중단하면 안 됩니다. 오류는 onWarn으로 보고하고 내장 압축으로 돌아갑니다.
// 활성화 성공 시 opts.Compaction을 비워 agentcore의 두 컨텍스트 관리자 동시 설정 경고를 방지합니다.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 압축 활성화 실패, 내장 압축으로 전환: " + err.Error())
		}
		return
	}
	// Compactor가 Compaction보다 우선하지만 함께 있으면 agentcore가 매번 경고하므로 명시적으로 비웁니다.
	opts.Compaction = nil
}
