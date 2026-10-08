@echo off
rem 비 ASCII 문자가 깨지지 않도록 콘솔을 UTF-8로 전환합니다.
chcp 65001 >nul 2>&1
rem ARTEX 감시 실행 스크립트(Windows)
rem
rem 사용법:
rem   start.bat                 포그라운드 실행(Ctrl-C로 중지)
rem   start.bat -addr :9000      추가 인자를 artex에 그대로 전달
rem
rem artex.exe를 실행하고 종료 코드에 따라 재시작 여부만 결정합니다.
rem
rem   0      사용자의 정상 종료 -> 반복 종료
rem   75     프로그램 재시작 요청 -> 즉시 재실행(페이지의 원클릭 업데이트 또는 롤백)
rem   기타   비정상 종료 -> 백오프 후 재실행(1->2->4…최대 60초)
rem
rem 다운로드, SHA256 검증, 교체는 artex가 시작 시 selfupdate 패키지에서 처리합니다.
rem 스크립트는 단순하게 유지합니다. 자세한 내용은 start.sh 상단 설명을 참조하세요.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 실행 파일을 찾을 수 없습니다: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 정상 종료
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem 업데이트/롤백 준비 완료: 재시작 시 artex가 교체를 완료합니다.
	echo [artex] 재시작 요청(새 버전 적용)…
	set /a delay=1
	goto loop
)

echo [artex] 비정상 종료 ^(code=!code!^), !delay!초 후 재시작 1>&2
rem 리디렉션된 콘솔에서는 timeout이 실패하므로 ping 사용(N초 대기는 N+1회 필요).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
