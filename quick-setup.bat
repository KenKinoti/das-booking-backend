@echo off
echo Setting up AGO CRM Backend...
echo.

REM Download and install TDM-GCC if not available
where gcc >nul 2>&1
if %errorlevel% neq 0 (
    echo Installing TDM-GCC...
    powershell -Command "Invoke-WebRequest -Uri 'https://github.com/jmeubank/tdm-gcc/releases/download/v10.3.0-tdm64-2/tdm64-gcc-10.3.0-2.exe' -OutFile 'tdm-gcc-installer.exe'"
    tdm-gcc-installer.exe /S
    set "PATH=%PATH%;C:\TDM-GCC-64\bin"
    del tdm-gcc-installer.exe
    echo TDM-GCC installed successfully!
) else (
    echo GCC is already available.
)

REM Set environment for CGO and development
set CGO_ENABLED=1
set ENVIRONMENT=development

echo.
echo Building and running the backend with database seeding...
go run main.go

pause