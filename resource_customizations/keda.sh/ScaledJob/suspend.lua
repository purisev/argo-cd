if obj.metadata.annotations == nil then
  obj.metadata.annotations = {}
end
obj.metadata.annotations["autoscaling.keda.sh/paused"] = "true"
return obj
