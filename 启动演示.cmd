@echo off
chcp 65001 >nul
cd /d "%~dp0"
go-monitor.exe serve -demo -db data/demo.db
pause
